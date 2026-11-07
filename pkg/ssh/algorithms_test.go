package ssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	gossh "golang.org/x/crypto/ssh"
)

// sha1Algorithms are the SHA-1 based algorithms x/crypto still offers by
// default and that the server must not offer unless configured to.
var sha1Algorithms = []string{
	gossh.InsecureKeyExchangeDH14SHA1,
	gossh.InsecureKeyExchangeDH1SHA1,
	gossh.InsecureKeyExchangeDHGEXSHA1,
	gossh.HMACSHA1,
	gossh.InsecureHMACSHA196,
}

func TestServerConfigExcludesSHA1ByDefault(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	sc := serverConfig(cfg, log.Default())
	if len(sc.KeyExchanges) == 0 || len(sc.Ciphers) == 0 || len(sc.MACs) == 0 {
		t.Fatalf("serverConfig leaves algorithms to x/crypto defaults: kex %v, ciphers %v, macs %v",
			sc.KeyExchanges, sc.Ciphers, sc.MACs)
	}
	for _, list := range [][]string{sc.KeyExchanges, sc.Ciphers, sc.MACs} {
		for _, a := range list {
			if slices.Contains(sha1Algorithms, a) {
				t.Errorf("serverConfig offers %q by default", a)
			}
		}
	}
	if !slices.Contains(sc.KeyExchanges, gossh.KeyExchangeCurve25519) {
		t.Errorf("default key exchanges %v lack %s", sc.KeyExchanges, gossh.KeyExchangeCurve25519)
	}
}

func TestServerConfigHonoursConfiguredAlgorithms(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.SSH.KexAlgorithms = []string{gossh.KeyExchangeCurve25519, gossh.InsecureKeyExchangeDH14SHA1}
	cfg.SSH.Ciphers = []string{gossh.CipherAES256CTR}
	cfg.SSH.MACs = []string{gossh.HMACSHA512, gossh.HMACSHA1}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	sc := serverConfig(cfg, log.Default())
	if !slices.Equal(sc.KeyExchanges, cfg.SSH.KexAlgorithms) {
		t.Errorf("key exchanges = %v, want %v", sc.KeyExchanges, cfg.SSH.KexAlgorithms)
	}
	if !slices.Equal(sc.Ciphers, cfg.SSH.Ciphers) {
		t.Errorf("ciphers = %v, want %v", sc.Ciphers, cfg.SSH.Ciphers)
	}
	if !slices.Equal(sc.MACs, cfg.SSH.MACs) {
		t.Errorf("macs = %v, want %v", sc.MACs, cfg.SSH.MACs)
	}
}

// startAlgorithmsServer starts a full SSH server built by NewSSHServer with
// cfg and returns its address.
func startAlgorithmsServer(t *testing.T, cfg *config.Config) string {
	t.Helper()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx := config.WithContext(context.Background(), cfg)
	ctx = log.WithContext(ctx, log.Default())
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbx.Close() })
	if err := migrate.Migrate(ctx, dbx); err != nil {
		t.Fatal(err)
	}
	ctx = db.WithContext(ctx, dbx)
	dbstore := database.New(ctx, dbx)
	ctx = store.WithContext(ctx, dbstore)
	be := backend.New(ctx, cfg, dbx, dbstore)
	ctx = backend.WithContext(ctx, be)

	s, err := NewSSHServer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(l) //nolint: errcheck
	t.Cleanup(func() { _ = s.Close() })
	return l.Addr().String()
}

// dialWith connects to addr with a client limited to the given algorithms
// (nil leaves x/crypto's client defaults) and returns the handshake error.
func dialWith(t *testing.T, addr string, kex, ciphers, macs []string) error {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	c, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		Config: gossh.Config{
			KeyExchanges: kex,
			Ciphers:      ciphers,
			MACs:         macs,
		},
		User:            "test",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), //nolint: gosec
		Timeout:         10 * time.Second,
	})
	if err == nil {
		_ = c.Close()
	}
	return err
}

func wantNoCommonAlgorithm(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: client connected, want no common algorithm", what)
		return
	}
	if !strings.Contains(err.Error(), "no common algorithm") {
		t.Errorf("%s: err = %v, want no common algorithm", what, err)
	}
}

func TestSSHServerRefusesSHA1ByDefault(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	addr := startAlgorithmsServer(t, cfg)

	// A non-AEAD cipher makes the MAC part of the negotiation.
	ctr := []string{gossh.CipherAES128CTR}

	if err := dialWith(t, addr, nil, nil, nil); err != nil {
		t.Fatalf("default client: %v", err)
	}
	if err := dialWith(t, addr, nil, ctr, []string{gossh.HMACSHA256}); err != nil {
		t.Fatalf("client with hmac-sha2-256: %v", err)
	}
	wantNoCommonAlgorithm(t, dialWith(t, addr, []string{gossh.InsecureKeyExchangeDH14SHA1}, nil, nil),
		"client limited to diffie-hellman-group14-sha1")
	wantNoCommonAlgorithm(t, dialWith(t, addr, []string{gossh.InsecureKeyExchangeDHGEXSHA1}, nil, nil),
		"client limited to diffie-hellman-group-exchange-sha1")
	wantNoCommonAlgorithm(t, dialWith(t, addr, nil, ctr, []string{gossh.HMACSHA1}),
		"client limited to hmac-sha1")
	wantNoCommonAlgorithm(t, dialWith(t, addr, nil, ctr, []string{gossh.InsecureHMACSHA196}),
		"client limited to hmac-sha1-96")
}

func TestSSHServerHonoursConfiguredAlgorithms(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.SSH.KexAlgorithms = []string{gossh.InsecureKeyExchangeDH14SHA1}
	cfg.SSH.Ciphers = []string{gossh.CipherAES128CTR}
	cfg.SSH.MACs = []string{gossh.HMACSHA1}
	addr := startAlgorithmsServer(t, cfg)

	if err := dialWith(t, addr, []string{gossh.InsecureKeyExchangeDH14SHA1}, nil, []string{gossh.HMACSHA1}); err != nil {
		t.Fatalf("client limited to the configured legacy algorithms: %v", err)
	}
	wantNoCommonAlgorithm(t, dialWith(t, addr, []string{gossh.KeyExchangeCurve25519}, nil, nil),
		"client limited to curve25519-sha256")
	wantNoCommonAlgorithm(t, dialWith(t, addr, nil, []string{gossh.CipherAES256GCM}, nil),
		"client limited to aes256-gcm")
	wantNoCommonAlgorithm(t, dialWith(t, addr, nil, nil, []string{gossh.HMACSHA256}),
		"client limited to hmac-sha2-256")
}
