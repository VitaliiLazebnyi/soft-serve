package web

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"
)

// Serve speaks TLS on the given listener when a TLS configuration is set, as
// ListenAndServe does, and plain HTTP otherwise.
func TestHTTPServerServe(t *testing.T) {
	cert := selfSignedCert(t)
	for _, useTLS := range []bool{false, true} {
		name := "plain"
		if useTLS {
			name = "tls"
		}
		t.Run(name, func(t *testing.T) {
			s := &HTTPServer{Server: &http.Server{
				Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_, _ = io.WriteString(w, "ok")
				}),
				ReadHeaderTimeout: 10 * time.Second,
			}}
			if useTLS {
				s.SetTLSConfig(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
			}

			l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			errc := make(chan error, 1)
			go func() { errc <- s.Serve(l) }()

			pool := x509.NewCertPool()
			pool.AddCert(cert.Leaf)
			client := &http.Client{
				Timeout:   10 * time.Second,
				Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
			}
			scheme := "http"
			if useTLS {
				scheme = "https"
			}
			resp, err := client.Get(scheme + "://" + l.Addr().String() + "/") //nolint:noctx
			if err != nil {
				t.Fatalf("GET over %s: %v", scheme, err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if string(body) != "ok" {
				t.Fatalf("body = %q, want %q", body, "ok")
			}
			if (resp.TLS != nil) != useTLS {
				t.Fatalf("response over TLS = %v, want %v", resp.TLS != nil, useTLS)
			}

			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
				t.Fatalf("Serve returned %v, want http.ErrServerClosed", err)
			}
		})
	}
}

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}
