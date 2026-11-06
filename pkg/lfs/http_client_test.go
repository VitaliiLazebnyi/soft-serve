package lfs

import (
	"net/url"
	"testing"
)

// TestNewHTTPClientSharesTransport checks that LFS clients share one HTTP
// client, and so one connection pool, instead of each building its own
// transport with its own idle connections.
func TestNewHTTPClientSharesTransport(t *testing.T) {
	a := newHTTPClient(&url.URL{Scheme: "https", Host: "a.example.com"})
	b := newHTTPClient(&url.URL{Scheme: "https", Host: "b.example.com"})

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
