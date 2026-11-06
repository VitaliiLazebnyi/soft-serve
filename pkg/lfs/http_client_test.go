package lfs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
)

const (
	testStallTimeout = 200 * time.Millisecond
	testChunkDelay   = 100 * time.Millisecond
	testChunks       = 8 // 800ms in all, well past testStallTimeout
)

// newLoopbackAdapter returns a basic transfer adapter whose client is set up
// by newTransferClient like the shared secureHTTPClient, starting from the
// same whole-request timeout as ssrf.NewSecureClient, but whose transport may
// reach the loopback test server, which the SSRF-safe dialer would refuse.
func newLoopbackAdapter(t *testing.T) *BasicTransferAdapter {
	t.Helper()
	old := stallTimeout
	stallTimeout = testStallTimeout
	t.Cleanup(func() { stallTimeout = old })

	tr := &http.Transport{DialContext: (&net.Dialer{}).DialContext}
	t.Cleanup(tr.CloseIdleConnections)
	client := newTransferClient(&http.Client{Timeout: ssrf.ClientTimeout, Transport: tr})
	return &BasicTransferAdapter{client}
}

func TestHTTPClientHasNoWholeRequestTimeout(t *testing.T) {
	c := newHTTPClient(&url.URL{})
	if c.client.Timeout != 0 {
		t.Fatalf("client.Timeout = %v, want 0: it cuts off any LFS transfer that takes longer", c.client.Timeout)
	}
}

// A download whose body keeps flowing for longer than the stall timeout must
// be read in full.
func TestHTTPClientSlowDownloadCompletes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		for range testChunks {
			_, _ = io.WriteString(w, "chunk")
			w.(http.Flusher).Flush()
			time.Sleep(testChunkDelay)
		}
	}))
	defer srv.Close()

	a := newLoopbackAdapter(t)
	rc, err := a.Download(context.Background(), Pointer{}, &Link{Href: srv.URL})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	defer rc.Close() //nolint:errcheck
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if want := strings.Repeat("chunk", testChunks); string(b) != want {
		t.Fatalf("body = %q, want %q", b, want)
	}
}

// slowReader yields its chunks with a delay before each.
type slowReader struct{ left int }

func (r *slowReader) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, io.EOF
	}
	time.Sleep(testChunkDelay)
	r.left--
	return copy(p, "chunk"), nil
}

// An upload whose body keeps flowing for longer than the stall timeout must
// succeed, even though nothing is read from the server meanwhile.
func TestHTTPClientSlowUploadCompletes(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := newLoopbackAdapter(t)
	p := Pointer{Size: int64(len("chunk") * testChunks)}
	if err := a.Upload(context.Background(), p, &slowReader{left: testChunks}, &Link{Href: srv.URL}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if want := strings.Repeat("chunk", testChunks); <-got != want {
		t.Fatal("server did not receive the whole upload")
	}
}

// A server that stops sending must still fail the transfer, both before the
// response headers and in the middle of the body.
func TestHTTPClientStalledTransferFails(t *testing.T) {
	for name, headers := range map[string]bool{"no headers": false, "stalled body": true} {
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if headers {
					w.Header().Set("Content-Length", "100")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, "partial")
					w.(http.Flusher).Flush()
				}
				<-release
			}))
			defer srv.Close()
			defer close(release)

			a := newLoopbackAdapter(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			rc, err := a.Download(ctx, Pointer{}, &Link{Href: srv.URL})
			if err == nil {
				_, err = io.ReadAll(rc)
				rc.Close() //nolint:errcheck,gosec
			}
			if err == nil {
				t.Fatal("transfer succeeded, want a stall error")
			}
			if ctx.Err() != nil {
				t.Fatal("transfer ran until the test context expired; stall timeout not applied")
			}
		})
	}
}

// closedAddr returns a loopback address nothing listens on, so a request
// to it fails with a *url.Error.
func closedAddr(t *testing.T) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() //nolint: errcheck
	return addr
}

// debugContext returns a context whose logger writes everything, down to
// debug, to the returned buffer.
func debugContext(t *testing.T) (context.Context, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger := log.New(&buf)
	logger.SetLevel(log.DebugLevel)
	return log.WithContext(context.Background(), logger), &buf
}

// plainClient is an httpClient that may reach loopback test servers, which
// the client from newHTTPClient refuses.
func plainClient(t *testing.T, endpoint string) *httpClient {
	t.Helper()
	ep, err := NewEndpoint(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{}
	return &httpClient{
		client:    c,
		endpoint:  ep,
		transfers: map[string]TransferAdapter{TransferBasic: &BasicTransferAdapter{c}},
	}
}

func assertNoSecrets(t *testing.T, what, s string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(s, secret) {
			t.Errorf("%s contains %q: %s", what, secret, s)
		}
	}
}

// discard is a DownloadCallback that ignores what it is given.
func discard(Pointer, io.ReadCloser, error) error { return nil }

var testPointer = Pointer{Oid: strings.Repeat("a", 64), Size: 1}

// TestBatchRedactsEndpoint checks that an endpoint's user info, which can be
// a token, is neither logged nor returned when the batch request fails.
func TestBatchRedactsEndpoint(t *testing.T) {
	ctx, logs := debugContext(t)
	c := plainClient(t, "http://TOKEN:PASSWORD@"+closedAddr(t)+"/x.git")

	err := c.Download(ctx, []Pointer{testPointer}, discard)
	if err == nil {
		t.Fatal("expected the batch request to fail")
	}
	var uerr *url.Error
	var operr *net.OpError
	if !errors.As(err, &uerr) || !errors.As(err, &operr) {
		t.Errorf("error lost its type: %#v", err)
	}
	assertNoSecrets(t, "error", err.Error(), "TOKEN", "PASSWORD")
	assertNoSecrets(t, "logs", logs.String(), "TOKEN", "PASSWORD")
	if !strings.Contains(logs.String(), "/x.git/info/lfs/objects/batch") {
		t.Errorf("logs no longer name the batch URL: %s", logs.String())
	}
}

// batchServer serves a batch response with the given actions for every
// object.
func batchServer(t *testing.T, actions map[string]*Link) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req BatchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		res := BatchResponse{Transfer: TransferBasic}
		for _, p := range req.Objects {
			res.Objects = append(res.Objects, &ObjectResponse{Pointer: p, Actions: actions})
		}
		w.Header().Set("Content-Type", MediaType)
		json.NewEncoder(w).Encode(res) //nolint: errcheck
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestTransferRedactsHref checks that a presigned href's signature and an
// action's headers are neither logged nor returned when a transfer fails.
func TestTransferRedactsHref(t *testing.T) {
	header := map[string]string{"Authorization": "Bearer HEADERSECRET"}
	tests := map[string]struct {
		actions  map[string]*Link
		wantURL  bool
		wantText string
	}{
		"request fails": {
			actions: map[string]*Link{ActionDownload: {
				Href:   fmt.Sprintf("http://USER@%s/obj?X-Amz-Signature=SIGNATURE", closedAddr(t)),
				Header: header,
			}},
			wantURL:  true,
			wantText: "/obj",
		},
		"href does not parse": {
			actions: map[string]*Link{ActionDownload: {
				Href:   "http://USER@127.0.0.1/obj%zz?X-Amz-Signature=SIGNATURE",
				Header: header,
			}},
			wantURL: true,
		},
		"missing action": {
			actions: map[string]*Link{ActionUpload: {
				Href:   "http://USER@127.0.0.1/obj?X-Amz-Signature=SIGNATURE",
				Header: header,
			}},
			wantText: "missing action 'download'",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			srv := batchServer(t, tt.actions)
			ctx, logs := debugContext(t)
			c := plainClient(t, srv.URL+"/x.git")

			err := c.Download(ctx, []Pointer{testPointer}, discard)
			if err == nil {
				t.Fatal("expected the download to fail")
			}
			var uerr *url.Error
			if tt.wantURL != errors.As(err, &uerr) {
				t.Errorf("error is a *url.Error = %v, want %v: %v", !tt.wantURL, tt.wantURL, err)
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error %q does not contain %q", err, tt.wantText)
			}
			assertNoSecrets(t, "error", err.Error(), "USER", "SIGNATURE", "HEADERSECRET", "%zz")
			assertNoSecrets(t, "logs", logs.String(), "USER", "SIGNATURE", "HEADERSECRET", "%zz")
		})
	}
}

// TestNewHTTPClientSharesTransport checks that LFS clients share one HTTP
// client, and so one connection pool, instead of each building its own
// transport with its own idle connections.
// The clients are built from two goroutines, as concurrent mirror jobs do, so
// that -race catches newHTTPClient changing the shared client.
func TestNewHTTPClientSharesTransport(t *testing.T) {
	var a, b *httpClient
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		a = newHTTPClient(&url.URL{Scheme: "https", Host: "a.example.com"})
	}()
	go func() {
		defer wg.Done()
		b = newHTTPClient(&url.URL{Scheme: "https", Host: "b.example.com"})
	}()
	wg.Wait()

	if a.client != b.client {
		t.Fatal("LFS clients use separate HTTP clients; want one shared client")
	}
	if a.client.Transport != b.client.Transport {
		t.Fatal("LFS clients use separate transports; want one shared transport")
	}
	basic, ok := a.transfers[TransferBasic].(*BasicTransferAdapter)
	if !ok {
		t.Fatalf("basic transfer adapter is %T, want *BasicTransferAdapter", a.transfers[TransferBasic])
	}
	if basic.client != a.client {
		t.Fatal("basic transfer adapter does not use the shared HTTP client")
	}
}

// setTransferTimeouts must refuse a transport it cannot add the stall limit
// to, rather than leave transfers with no bound at all.
func TestSetTransferTimeoutsPanicsOnUnknownTransport(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("setTransferTimeouts accepted a transport it cannot wrap")
		}
	}()
	setTransferTimeouts(&http.Client{Transport: http.NewFileTransport(http.Dir("."))})
}

// The shared client must still be the SSRF-protected one: it refuses to
// connect to loopback and does not follow redirects.
func TestSecureHTTPClientIsSSRFProtected(t *testing.T) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:1/", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := secureHTTPClient.Do(req)
	if err == nil {
		res.Body.Close() //nolint:errcheck,gosec
	}
	if !errors.Is(err, ssrf.ErrPrivateIP) {
		t.Fatalf("request to loopback: err = %v, want %v", err, ssrf.ErrPrivateIP)
	}

	if cr := secureHTTPClient.CheckRedirect; cr == nil || !errors.Is(cr(req, []*http.Request{req}), http.ErrUseLastResponse) {
		t.Fatal("shared client follows redirects; want it to return the redirect response")
	}
}
