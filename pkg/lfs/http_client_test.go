package lfs

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/ssrf"
)

const (
	testStallTimeout = 200 * time.Millisecond
	testChunkDelay   = 100 * time.Millisecond
	testChunks       = 8 // 800ms in all, well past testStallTimeout
)

// newLoopbackAdapter returns a basic transfer adapter whose client is set up
// like the one newHTTPClient builds, with the same whole-request timeout as
// ssrf.NewSecureClient, but whose transport may reach the loopback test
// server, which the SSRF-safe dialer would refuse.
func newLoopbackAdapter(t *testing.T) *BasicTransferAdapter {
	t.Helper()
	old := stallTimeout
	stallTimeout = testStallTimeout
	t.Cleanup(func() { stallTimeout = old })

	tr := &http.Transport{DialContext: (&net.Dialer{}).DialContext}
	t.Cleanup(tr.CloseIdleConnections)
	client := &http.Client{Timeout: ssrf.ClientTimeout, Transport: tr}
	setTransferTimeouts(client)
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
