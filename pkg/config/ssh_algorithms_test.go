package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/matryer/is"
	"go.yaml.in/yaml/v4"
	"golang.org/x/crypto/ssh"
)

func TestSSHAlgorithmDefaultsExcludeSHA1(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.SSH.KexAlgorithms)+len(cfg.SSH.Ciphers)+len(cfg.SSH.MACs) != 0 {
		t.Fatalf("algorithm lists set by default: %v %v %v", cfg.SSH.KexAlgorithms, cfg.SSH.Ciphers, cfg.SSH.MACs)
	}
	sha1 := []string{
		ssh.InsecureKeyExchangeDH14SHA1, ssh.InsecureKeyExchangeDH1SHA1, ssh.InsecureKeyExchangeDHGEXSHA1,
		ssh.HMACSHA1, ssh.InsecureHMACSHA196,
	}
	for _, list := range [][]string{cfg.SSH.SSHKexAlgorithms(), cfg.SSH.SSHCiphers(), cfg.SSH.SSHMACs()} {
		if len(list) == 0 {
			t.Error("empty default algorithm list")
		}
		for _, a := range list {
			if slices.Contains(sha1, a) {
				t.Errorf("default algorithms include %q", a)
			}
		}
	}
	if !slices.Equal(cfg.SSH.SSHCiphers(), ssh.SupportedAlgorithms().Ciphers) {
		t.Errorf("default ciphers = %v, want x/crypto's supported ciphers", cfg.SSH.SSHCiphers())
	}
}

func TestParseSSHAlgorithms(t *testing.T) {
	is := is.New(t)
	for _, k := range []string{"SOFT_SERVE_SSH_KEX_ALGORITHMS", "SOFT_SERVE_SSH_CIPHERS", "SOFT_SERVE_SSH_MACS"} {
		t.Setenv(k, "")
		is.NoErr(os.Unsetenv(k))
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	is.NoErr(os.WriteFile(path, []byte(`ssh:
  kex_algorithms: ["curve25519-sha256", "diffie-hellman-group14-sha1"]
  ciphers: ["aes256-gcm@openssh.com"]
  macs: ["hmac-sha2-512", "hmac-sha1"]
`), 0o600))
	cfg := DefaultConfig()
	cfg.DataPath = dir
	is.NoErr(parseFile(cfg, path))
	is.Equal(cfg.SSH.KexAlgorithms, []string{"curve25519-sha256", "diffie-hellman-group14-sha1"})
	is.Equal(cfg.SSH.SSHKexAlgorithms(), []string{"curve25519-sha256", "diffie-hellman-group14-sha1"})
	is.Equal(cfg.SSH.SSHCiphers(), []string{"aes256-gcm@openssh.com"})
	is.Equal(cfg.SSH.SSHMACs(), []string{"hmac-sha2-512", "hmac-sha1"})

	t.Setenv("SOFT_SERVE_SSH_KEX_ALGORITHMS", "curve25519-sha256@libssh.org, curve25519-sha256")
	t.Setenv("SOFT_SERVE_SSH_CIPHERS", "aes128-ctr,aes256-ctr")
	t.Setenv("SOFT_SERVE_SSH_MACS", "hmac-sha2-256-etm@openssh.com")
	cfg = DefaultConfig()
	cfg.DataPath = dir
	is.NoErr(cfg.ParseEnv())
	is.Equal(cfg.SSH.SSHKexAlgorithms(), []string{"curve25519-sha256@libssh.org", "curve25519-sha256"})
	is.Equal(cfg.SSH.SSHCiphers(), []string{"aes128-ctr", "aes256-ctr"})
	is.Equal(cfg.SSH.SSHMACs(), []string{"hmac-sha2-256-etm@openssh.com"})

	// Hooks re-read the config from the environment.
	env := cfg.Environ()
	is.True(slices.Contains(env, "SOFT_SERVE_SSH_KEX_ALGORITHMS=curve25519-sha256@libssh.org,curve25519-sha256"))
	is.True(slices.Contains(env, "SOFT_SERVE_SSH_CIPHERS=aes128-ctr,aes256-ctr"))
	is.True(slices.Contains(env, "SOFT_SERVE_SSH_MACS=hmac-sha2-256-etm@openssh.com"))

	// An empty variable, as Environ writes for an unset list, keeps the
	// default rather than reading as one empty name.
	t.Setenv("SOFT_SERVE_SSH_MACS", "")
	cfg = DefaultConfig()
	cfg.DataPath = dir
	is.NoErr(cfg.ParseEnv())
	is.Equal(len(cfg.SSH.MACs), 0)
	is.Equal(cfg.SSH.SSHMACs(), DefaultSSHMACs())
}

func TestValidateRejectsUnknownSSHAlgorithms(t *testing.T) {
	for _, tt := range []struct {
		field string
		set   func(*SSHConfig)
	}{
		{"ssh.kex_algorithms", func(c *SSHConfig) { c.KexAlgorithms = []string{"curve25519-sha256", "curve25519-sha512"} }},
		{"ssh.ciphers", func(c *SSHConfig) { c.Ciphers = []string{"aes128-gmc@openssh.com"} }},
		{"ssh.macs", func(c *SSHConfig) { c.MACs = []string{"hmac-md5"} }},
		// A name valid in one list is not valid in another.
		{"ssh.macs", func(c *SSHConfig) { c.MACs = []string{"aes128-ctr"} }},
	} {
		cfg := DefaultConfig()
		cfg.DataPath = t.TempDir()
		tt.set(&cfg.SSH)
		err := cfg.Validate()
		if err == nil {
			t.Errorf("%s: Validate accepted an unknown algorithm", tt.field)
			continue
		}
		if !strings.Contains(err.Error(), tt.field) || !strings.Contains(err.Error(), "unknown algorithm") {
			t.Errorf("%s: unclear error %q", tt.field, err)
		}
	}

	// Legacy algorithms x/crypto still implements are accepted, for
	// operators who must re-enable them.
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.SSH.KexAlgorithms = ssh.InsecureAlgorithms().KeyExchanges
	cfg.SSH.Ciphers = ssh.InsecureAlgorithms().Ciphers
	cfg.SSH.MACs = ssh.InsecureAlgorithms().MACs
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate rejected legacy algorithms: %v", err)
	}
}

func TestSSHAlgorithmsConfigFileRoundTrip(t *testing.T) {
	is := is.New(t)
	for _, want := range [][]string{nil, {"hmac-sha2-256", "hmac-sha1"}} {
		cfg := DefaultConfig()
		cfg.DataPath = t.TempDir()
		cfg.SSH.MACs = want
		path := filepath.Join(cfg.DataPath, "config.yaml")
		is.NoErr(writeConfig(cfg, path))

		got := DefaultConfig()
		got.DataPath = cfg.DataPath
		is.NoErr(parseFile(got, path))
		is.Equal(len(got.SSH.KexAlgorithms), 0)
		is.Equal(len(got.SSH.Ciphers), 0)
		is.Equal(len(got.SSH.MACs), len(want))
		if len(want) > 0 {
			is.Equal(got.SSH.MACs, want)
		}
	}
}

// TestSSHAlgorithmDocsMatchDefaults keeps the default lists and the legacy
// examples in the README and the generated config file in step with the
// defaults the server uses.
func TestSSHAlgorithmDocsMatchDefaults(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(readme)

	// The bullet list of defaults.
	for _, l := range []struct {
		prefix string
		want   []string
	}{
		{"- key exchange: ", DefaultSSHKexAlgorithms()},
		{"- ciphers: ", DefaultSSHCiphers()},
		{"- MACs: ", DefaultSSHMACs()},
	} {
		i := strings.Index(doc, l.prefix)
		if i < 0 {
			t.Fatalf("README has no %q line", l.prefix)
		}
		line := doc[i+len(l.prefix):]
		line = line[:strings.IndexByte(line, '\n')]
		line = strings.ReplaceAll(line, " (and its alias `curve25519-sha256@libssh.org`)", "")
		var got []string
		for _, n := range strings.Split(line, ", ") {
			got = append(got, strings.Trim(n, "`"))
		}
		if !slices.Equal(got, l.want) {
			t.Errorf("README %q lists %v, want %v", l.prefix, got, l.want)
		}
	}

	wantKex := append(DefaultSSHKexAlgorithms(), ssh.InsecureKeyExchangeDH14SHA1)
	wantMACs := append(DefaultSSHMACs(), ssh.HMACSHA1)
	check := func(name, example string) {
		t.Helper()
		var cfg Config
		if err := yaml.Unmarshal([]byte(example), &cfg); err != nil {
			t.Fatalf("%s legacy example: %v", name, err)
		}
		if !slices.Equal(cfg.SSH.KexAlgorithms, wantKex) {
			t.Errorf("%s legacy example kex_algorithms = %v, want %v", name, cfg.SSH.KexAlgorithms, wantKex)
		}
		if !slices.Equal(cfg.SSH.MACs, wantMACs) {
			t.Errorf("%s legacy example macs = %v, want %v", name, cfg.SSH.MACs, wantMACs)
		}
	}

	// The README legacy example is the yaml block after "Older clients".
	rest := doc[strings.Index(doc, "**Older clients.**"):]
	start := strings.Index(rest, "```yaml\n") + len("```yaml\n")
	check("README", rest[start:start+strings.Index(rest[start:], "```")])

	// The generated config file has it as comment lines after
	// "append the legacy algorithm".
	file := newConfigFile(DefaultConfig())
	var b strings.Builder
	b.WriteString("ssh:\n")
	lines := strings.Split(file[strings.Index(file, "append the legacy algorithm"):], "\n")[2:]
	for _, l := range lines {
		if !strings.HasPrefix(l, "  #   ") {
			break
		}
		b.WriteString("  " + strings.TrimPrefix(l, "  #   ") + "\n")
	}
	check("config file", b.String())
}
