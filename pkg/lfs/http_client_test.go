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
	"testing"

	"charm.land/log/v2"
)

// closedAddr returns a loopback address nothing listens on, so a request
// to it fails with a *url.Error.
func closedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
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
