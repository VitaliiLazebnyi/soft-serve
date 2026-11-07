package config

import (
	"testing"

	"go.yaml.in/yaml/v4"
)

func TestNewConfigFile(t *testing.T) {
	for _, cfg := range []*Config{
		nil,
		DefaultConfig(),
		{},
	} {
		if s := newConfigFile(cfg); s == "" {
			t.Errorf("newConfigFile(nil) => %q, want non-empty string", s)
		}
	}
}

// TestNewConfigFileRoundTrip checks that every string the config file
// template writes reads back unchanged, on every platform. Windows paths are
// the common case: in a double-quoted YAML scalar, C:\a\x1 would otherwise
// read as the escapes \a and \x1.
func TestNewConfigFileRoundTrip(t *testing.T) {
	const winPath = `D:\a\soft-serve\x1\data\soft-serve.db?_pragma=busy_timeout(5000)`
	odd := "quote \" backslash \\ hash # colon: tab\t bell\a nel\u0085 ls\u2028 nonchar\ufffe\uffff é"

	cfg := DefaultConfig()
	cfg.Name = odd
	cfg.Log.Format = odd
	cfg.Log.TimeFormat = odd
	cfg.SSH.ListenAddr = odd
	cfg.SSH.PublicURL = odd
	cfg.SSH.KeyPath = `C:\Users\me\ssh\soft_serve_host_ed25519`
	cfg.SSH.ClientKeyPath = `\\server\share\#key: x`
	cfg.SSH.ClientStrictHostKeyChecking = odd
	cfg.SSH.ClientAllowedHosts = []string{odd, `C:\x`}
	cfg.Git.ListenAddr = odd
	cfg.Git.PublicURL = odd
	cfg.HTTP.ListenAddr = odd
	cfg.HTTP.TLSKeyPath = `C:\tls\key.pem`
	cfg.HTTP.TLSCertPath = `"C:\tls\cert.pem"`
	cfg.HTTP.PublicURL = odd
	cfg.Stats.ListenAddr = odd
	cfg.DB.Driver = odd
	cfg.DB.DataSource = winPath
	cfg.Jobs.MirrorPull = odd
	cfg.Webhook.AllowedHosts = []string{odd, `C:\y`}

	var got Config
	if err := yaml.Unmarshal([]byte(newConfigFile(cfg)), &got); err != nil {
		t.Fatalf("decode written config: %v", err)
	}

	for _, c := range []struct{ name, got, want string }{
		{"name", got.Name, cfg.Name},
		{"log.format", got.Log.Format, cfg.Log.Format},
		{"log.time_format", got.Log.TimeFormat, cfg.Log.TimeFormat},
		{"ssh.listen_addr", got.SSH.ListenAddr, cfg.SSH.ListenAddr},
		{"ssh.public_url", got.SSH.PublicURL, cfg.SSH.PublicURL},
		{"ssh.key_path", got.SSH.KeyPath, cfg.SSH.KeyPath},
		{"ssh.client_key_path", got.SSH.ClientKeyPath, cfg.SSH.ClientKeyPath},
		{"ssh.client_strict_host_key_checking", got.SSH.ClientStrictHostKeyChecking, cfg.SSH.ClientStrictHostKeyChecking},
		{"git.listen_addr", got.Git.ListenAddr, cfg.Git.ListenAddr},
		{"git.public_url", got.Git.PublicURL, cfg.Git.PublicURL},
		{"http.listen_addr", got.HTTP.ListenAddr, cfg.HTTP.ListenAddr},
		{"http.tls_key_path", got.HTTP.TLSKeyPath, cfg.HTTP.TLSKeyPath},
		{"http.tls_cert_path", got.HTTP.TLSCertPath, cfg.HTTP.TLSCertPath},
		{"http.public_url", got.HTTP.PublicURL, cfg.HTTP.PublicURL},
		{"stats.listen_addr", got.Stats.ListenAddr, cfg.Stats.ListenAddr},
		{"db.driver", got.DB.Driver, cfg.DB.Driver},
		{"db.data_source", got.DB.DataSource, cfg.DB.DataSource},
		{"jobs.mirror_pull", got.Jobs.MirrorPull, cfg.Jobs.MirrorPull},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	for _, c := range []struct {
		name      string
		got, want []string
	}{
		{"ssh.client_allowed_hosts", got.SSH.ClientAllowedHosts, cfg.SSH.ClientAllowedHosts},
		{"webhook.allowed_hosts", got.Webhook.AllowedHosts, cfg.Webhook.AllowedHosts},
		{"http.cors.allowed_origins", got.HTTP.CORS.AllowedOrigins, []string{cfg.HTTP.PublicURL}},
	} {
		if len(c.got) != len(c.want) {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
			continue
		}
		for i := range c.got {
			if c.got[i] != c.want[i] {
				t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
				break
			}
		}
	}
}
