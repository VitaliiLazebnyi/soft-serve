package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/matryer/is"
)

var dbPoolEnv = []string{
	"SOFT_SERVE_DB_MAX_OPEN_CONNS",
	"SOFT_SERVE_DB_MAX_IDLE_CONNS",
	"SOFT_SERVE_DB_CONN_MAX_LIFETIME",
	"SOFT_SERVE_DB_CONN_MAX_IDLE_TIME",
}

func TestDBPoolLimits(t *testing.T) {
	is := is.New(t)
	// Ignore any value exported in the environment running the tests.
	for _, k := range dbPoolEnv {
		t.Setenv(k, "")
		is.NoErr(os.Unsetenv(k))
	}
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	is.Equal(cfg.DB.MaxOpenConns, 0)
	is.Equal(cfg.DB.MaxIdleConns, 0)
	is.Equal(cfg.DB.ConnMaxLifetime, 0)
	is.Equal(cfg.DB.ConnMaxIdleTime, 0)

	t.Setenv("SOFT_SERVE_DB_MAX_OPEN_CONNS", "20")
	t.Setenv("SOFT_SERVE_DB_MAX_IDLE_CONNS", "5")
	t.Setenv("SOFT_SERVE_DB_CONN_MAX_LIFETIME", "1800")
	t.Setenv("SOFT_SERVE_DB_CONN_MAX_IDLE_TIME", "300")
	cfg = DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	is.Equal(cfg.DB.MaxOpenConns, 20)
	is.Equal(cfg.DB.MaxIdleConns, 5)
	is.Equal(cfg.DB.ConnMaxLifetime, 1800)
	is.Equal(cfg.DB.ConnMaxIdleTime, 300)
	// Hooks re-read the config from the environment.
	env := cfg.Environ()
	for _, want := range []string{
		"SOFT_SERVE_DB_MAX_OPEN_CONNS=20",
		"SOFT_SERVE_DB_MAX_IDLE_CONNS=5",
		"SOFT_SERVE_DB_CONN_MAX_LIFETIME=1800",
		"SOFT_SERVE_DB_CONN_MAX_IDLE_TIME=300",
	} {
		is.True(slices.Contains(env, want))
	}
	for _, k := range dbPoolEnv {
		is.NoErr(os.Unsetenv(k))
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	is.NoErr(os.WriteFile(path, []byte(`db:
  max_open_conns: 10
  max_idle_conns: 4
  conn_max_lifetime: 600
  conn_max_idle_time: 60
`), 0o600))
	cfg = DefaultConfig()
	cfg.DataPath = dir
	is.NoErr(parseFile(cfg, path))
	is.Equal(cfg.DB.MaxOpenConns, 10)
	is.Equal(cfg.DB.MaxIdleConns, 4)
	is.Equal(cfg.DB.ConnMaxLifetime, 600)
	is.Equal(cfg.DB.ConnMaxIdleTime, 60)

	// The generated config file keeps the values.
	out := filepath.Join(dir, "written.yaml")
	is.NoErr(writeConfig(cfg, out))
	got := DefaultConfig()
	got.DataPath = dir
	is.NoErr(parseFile(got, out))
	is.Equal(got.DB.MaxOpenConns, 10)
	is.Equal(got.DB.MaxIdleConns, 4)
	is.Equal(got.DB.ConnMaxLifetime, 600)
	is.Equal(got.DB.ConnMaxIdleTime, 60)
}

func TestDBPoolLimitsRejectNegative(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*DBConfig)
	}{
		{"db.max_open_conns", func(c *DBConfig) { c.MaxOpenConns = -1 }},
		{"db.max_idle_conns", func(c *DBConfig) { c.MaxIdleConns = -1 }},
		{"db.conn_max_lifetime", func(c *DBConfig) { c.ConnMaxLifetime = -1 }},
		{"db.conn_max_idle_time", func(c *DBConfig) { c.ConnMaxIdleTime = -1 }},
	} {
		cfg := DefaultConfig()
		cfg.DataPath = t.TempDir()
		tc.set(&cfg.DB)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.name) {
			t.Errorf("%s: Validate() = %v, want an error naming it", tc.name, err)
		}
	}
}
