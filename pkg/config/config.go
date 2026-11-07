package config

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/caarlos0/env/v11"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
	"go.yaml.in/yaml/v4"
	"golang.org/x/crypto/ssh"
)

var binPath = "soft"

const (
	// StrictHostKeyCheckingAcceptNew records an unknown host key on first
	// contact and refuses a changed one afterwards.
	StrictHostKeyCheckingAcceptNew = "accept-new"
	// StrictHostKeyCheckingYes refuses any host not already in known_hosts.
	StrictHostKeyCheckingYes = "yes"
)

// SSHConfig is the configuration for the SSH server.
type SSHConfig struct {
	// Enabled toggles the SSH server on/off
	Enabled bool `env:"ENABLED" yaml:"enabled"`

	// ListenAddr is the address on which the SSH server will listen.
	ListenAddr string `env:"LISTEN_ADDR" yaml:"listen_addr"`

	// PublicURL is the public URL of the SSH server.
	PublicURL string `env:"PUBLIC_URL" yaml:"public_url"`

	// KeyPath is the path to the SSH server's private key.
	KeyPath string `env:"KEY_PATH" yaml:"key_path"`

	// ClientKeyPath is the path to the server's client private key.
	ClientKeyPath string `env:"CLIENT_KEY_PATH" yaml:"client_key_path"`

	// ClientStrictHostKeyChecking is the StrictHostKeyChecking value the
	// server passes to ssh when cloning or syncing from ssh remotes. Valid
	// values are "accept-new" (record a host key on first contact, refuse a
	// changed one) and "yes" (only hosts already in known_hosts).
	ClientStrictHostKeyChecking string `env:"CLIENT_STRICT_HOST_KEY_CHECKING" yaml:"client_strict_host_key_checking"`

	// ClientAllowedHosts lists the ssh hosts users may import from or mirror.
	// The client key is presented to every one of them. Entries are
	// hostnames, IP addresses, or CIDR ranges with an optional port. When
	// empty, only server admins may use ssh remotes; when not, it binds
	// admins too.
	ClientAllowedHosts []string `env:"CLIENT_ALLOWED_HOSTS" yaml:"client_allowed_hosts"`

	// MaxTimeout is the maximum number of seconds a connection can take.
	MaxTimeout int `env:"MAX_TIMEOUT" yaml:"max_timeout"`

	// IdleTimeout is the number of seconds a connection can be idle before it is closed.
	IdleTimeout int `env:"IDLE_TIMEOUT" yaml:"idle_timeout"`

	// allowedHosts is ClientAllowedHosts parsed by Validate.
	allowedHosts ssrf.AllowedHosts
}

// AllowedHosts returns the parsed ssh allowlist. It is empty until Validate
// has run.
func (c *SSHConfig) AllowedHosts() ssrf.AllowedHosts {
	return c.allowedHosts
}

// GitConfig is the Git daemon configuration for the server.
type GitConfig struct {
	// Enabled toggles the Git daemon on/off
	Enabled bool `env:"ENABLED" yaml:"enabled"`

	// ListenAddr is the address on which the Git daemon will listen.
	ListenAddr string `env:"LISTEN_ADDR" yaml:"listen_addr"`

	// PublicURL is the public URL of the Git daemon server.
	PublicURL string `env:"PUBLIC_URL" yaml:"public_url"`

	// MaxTimeout is the maximum number of seconds a connection can take.
	MaxTimeout int `env:"MAX_TIMEOUT" yaml:"max_timeout"`

	// IdleTimeout is the number of seconds a connection can be idle before it is closed.
	IdleTimeout int `env:"IDLE_TIMEOUT" yaml:"idle_timeout"`

	// MaxConnections is the maximum number of concurrent connections.
	MaxConnections int `env:"MAX_CONNECTIONS" yaml:"max_connections"`
}

// CORSConfig is the CORS configuration for the server.
type CORSConfig struct {
	AllowedHeaders []string `env:"ALLOWED_HEADERS" yaml:"allowed_headers"`

	AllowedOrigins []string `env:"ALLOWED_ORIGINS" yaml:"allowed_origins"`

	AllowedMethods []string `env:"ALLOWED_METHODS" yaml:"allowed_methods"`
}

// HTTPConfig is the HTTP configuration for the server.
type HTTPConfig struct {
	// Enabled toggles the HTTP server on/off
	Enabled bool `env:"ENABLED" yaml:"enabled"`

	// ListenAddr is the address on which the HTTP server will listen.
	ListenAddr string `env:"LISTEN_ADDR" yaml:"listen_addr"`

	// TLSKeyPath is the path to the TLS private key.
	TLSKeyPath string `env:"TLS_KEY_PATH" yaml:"tls_key_path"`

	// TLSCertPath is the path to the TLS certificate.
	TLSCertPath string `env:"TLS_CERT_PATH" yaml:"tls_cert_path"`

	// PublicURL is the public URL of the HTTP server.
	PublicURL string `env:"PUBLIC_URL" yaml:"public_url"`

	// CORS is the cross-origin configuration for the HTTP server.
	CORS CORSConfig `envPrefix:"CORS_" yaml:"cors"`
}

// StatsConfig is the configuration for the stats server.
type StatsConfig struct {
	// Enabled toggles the Stats server on/off
	Enabled bool `env:"ENABLED" yaml:"enabled"`

	// ListenAddr is the address on which the stats server will listen.
	ListenAddr string `env:"LISTEN_ADDR" yaml:"listen_addr"`
}

// LogConfig is the logger configuration.
type LogConfig struct {
	// Format is the format of the logs.
	// Valid values are "json", "logfmt", and "text".
	Format string `env:"FORMAT" yaml:"format"`

	// Time format for the log `ts` field.
	// Format must be described in Golang's time format.
	TimeFormat string `env:"TIME_FORMAT" yaml:"time_format"`

	// Path to a file to write logs to.
	// If not set, logs will be written to stderr.
	Path string `env:"PATH" yaml:"path"`
}

// DBConfig is the database connection configuration.
type DBConfig struct {
	// Driver is the driver for the database.
	Driver string `env:"DRIVER" yaml:"driver"`

	// DataSource is the database data source name.
	DataSource string `env:"DATA_SOURCE" yaml:"data_source"`

	// MaxOpenConns is the maximum number of open connections to the
	// database. 0 (the default) means no limit.
	MaxOpenConns int `env:"MAX_OPEN_CONNS" yaml:"max_open_conns"`

	// MaxIdleConns is the maximum number of idle connections kept in the
	// pool. 0 (the default) keeps the database/sql default of 2. It is
	// capped at MaxOpenConns when that is set.
	MaxIdleConns int `env:"MAX_IDLE_CONNS" yaml:"max_idle_conns"`

	// ConnMaxLifetime is the maximum number of seconds a connection may be
	// reused. 0 (the default) means no limit.
	ConnMaxLifetime int `env:"CONN_MAX_LIFETIME" yaml:"conn_max_lifetime"`

	// ConnMaxIdleTime is the maximum number of seconds a connection may sit
	// idle before it is closed. 0 (the default) means no limit.
	ConnMaxIdleTime int `env:"CONN_MAX_IDLE_TIME" yaml:"conn_max_idle_time"`
}

// LFSConfig is the configuration for Git LFS.
type LFSConfig struct {
	// Enabled is whether or not Git LFS is enabled.
	Enabled bool `env:"ENABLED" yaml:"enabled"`

	// SSHEnabled is whether or not Git LFS over SSH is enabled.
	// This is only used if LFS is enabled.
	SSHEnabled bool `env:"SSH_ENABLED" yaml:"ssh_enabled"`
}

// JobsConfig is the configuration for cron jobs.
type JobsConfig struct {
	MirrorPull string `env:"MIRROR_PULL" yaml:"mirror_pull"`
}

// Config is the configuration for Soft Serve.
type Config struct {
	// Name is the name of the server.
	Name string `env:"NAME" yaml:"name"`

	// SSH is the configuration for the SSH server.
	SSH SSHConfig `envPrefix:"SSH_" yaml:"ssh"`

	// Git is the configuration for the Git daemon.
	Git GitConfig `envPrefix:"GIT_" yaml:"git"`

	// HTTP is the configuration for the HTTP server.
	HTTP HTTPConfig `envPrefix:"HTTP_" yaml:"http"`

	// Stats is the configuration for the stats server.
	Stats StatsConfig `envPrefix:"STATS_" yaml:"stats"`

	// Log is the logger configuration.
	Log LogConfig `envPrefix:"LOG_" yaml:"log"`

	// DB is the database configuration.
	DB DBConfig `envPrefix:"DB_" yaml:"db"`

	// LFS is the configuration for Git LFS.
	LFS LFSConfig `envPrefix:"LFS_" yaml:"lfs"`

	// Jobs is the configuration for cron jobs
	Jobs JobsConfig `envPrefix:"JOBS_" yaml:"jobs"`

	// InitialAdminKeys is a list of public keys that will be added to the list of admins.
	InitialAdminKeys []string `env:"INITIAL_ADMIN_KEYS" envSeparator:"\n" yaml:"initial_admin_keys"`

	// AnonAccess overrides the access level for anonymous users.
	//
	// If set (non-nil), this takes precedence over the "anon-access" value
	// stored in the database (see the `settings` command) on every read, for
	// as long as it remains set. It is not a one-time default: it behaves
	// like InitialAdminKeys, not like a seed value. Invalid values are
	// rejected at parse time by AccessLevel's TextUnmarshaler.
	//
	// This is intended for scripted, non-production bootstrapping only.
	AnonAccess *access.AccessLevel `env:"ANON_ACCESS" yaml:"anon_access"`

	// AllowKeyless overrides whether keyless (no public key) connections
	// are allowed.
	//
	// If set (non-nil), this takes precedence over the "allow-keyless"
	// value stored in the database on every read, same as AnonAccess above.
	// A nil value means "no override": fall back to the database.
	AllowKeyless *bool `env:"ALLOW_KEYLESS" yaml:"allow_keyless"`

	// AdminOnlyCreate restricts repository creation to server admins.
	//
	// When true, registered non-admins and anonymous users get read-only on
	// repository names that do not exist yet, so repo create, repo import,
	// push-to-create and rename-to-a-new-name are refused. Existing
	// repositories are not affected. When false (the default), access follows
	// upstream Soft Serve: registered users may create repositories.
	AdminOnlyCreate bool `env:"ADMIN_ONLY_CREATE" yaml:"admin_only_create"`

	// MaxReposPerUser caps how many repositories each user may create, for
	// every way of creating one: repo create, repo import and
	// push-to-create. Server admins (admin accounts or initial_admin_keys)
	// are exempt. Repositories created without an account all count
	// together as one user. A user at the limit is refused; the
	// repositories they already have are kept. 0 (the default) means no
	// limit.
	MaxReposPerUser int `env:"MAX_REPOS_PER_USER" yaml:"max_repos_per_user"`

	// DefaultRepo is a repository name to create on boot if it does not
	// already exist. Leave empty to disable.
	DefaultRepo string `env:"DEFAULT_REPO" yaml:"default_repo"`

	// DataPath is the path to the directory where Soft Serve will store its data.
	DataPath string `env:"DATA_PATH" yaml:"-"`

	// unreadableAdminKeyFiles and ignoredAdminKeys record the
	// InitialAdminKeys entries Validate dropped: key files that could not
	// be read or parsed, and entries that are neither a key nor a file path.
	// Validate rewrites InitialAdminKeys to the valid keys only, so these
	// are the only record of them. They are sets because Validate runs once
	// per config source, and more than once for the environment.
	unreadableAdminKeyFiles map[string]struct{}
	ignoredAdminKeys        map[string]struct{}
}

// Environ returns the config as a list of environment variables.
func (c *Config) Environ() []string {
	envs := []string{
		fmt.Sprintf("SOFT_SERVE_BIN_PATH=%s", binPath),
	}
	if c == nil {
		return envs
	}

	// TODO: do this dynamically
	envs = append(envs, []string{
		fmt.Sprintf("SOFT_SERVE_CONFIG_LOCATION=%s", c.ConfigPath()),
		fmt.Sprintf("SOFT_SERVE_DATA_PATH=%s", c.DataPath),
		fmt.Sprintf("SOFT_SERVE_NAME=%s", c.Name),
		fmt.Sprintf("SOFT_SERVE_INITIAL_ADMIN_KEYS=%s", strings.Join(c.InitialAdminKeys, "\n")),
		fmt.Sprintf("SOFT_SERVE_DEFAULT_REPO=%s", c.DefaultRepo),
		fmt.Sprintf("SOFT_SERVE_SSH_ENABLED=%t", c.SSH.Enabled),
		fmt.Sprintf("SOFT_SERVE_SSH_LISTEN_ADDR=%s", c.SSH.ListenAddr),
		fmt.Sprintf("SOFT_SERVE_SSH_PUBLIC_URL=%s", c.SSH.PublicURL),
		fmt.Sprintf("SOFT_SERVE_SSH_KEY_PATH=%s", c.SSH.KeyPath),
		fmt.Sprintf("SOFT_SERVE_SSH_CLIENT_KEY_PATH=%s", c.SSH.ClientKeyPath),
		fmt.Sprintf("SOFT_SERVE_SSH_CLIENT_STRICT_HOST_KEY_CHECKING=%s", c.SSH.ClientStrictHostKeyChecking),
		fmt.Sprintf("SOFT_SERVE_SSH_CLIENT_ALLOWED_HOSTS=%s", strings.Join(c.SSH.ClientAllowedHosts, ",")),
		fmt.Sprintf("SOFT_SERVE_SSH_MAX_TIMEOUT=%d", c.SSH.MaxTimeout),
		fmt.Sprintf("SOFT_SERVE_SSH_IDLE_TIMEOUT=%d", c.SSH.IdleTimeout),
		fmt.Sprintf("SOFT_SERVE_GIT_ENABLED=%t", c.Git.Enabled),
		fmt.Sprintf("SOFT_SERVE_GIT_LISTEN_ADDR=%s", c.Git.ListenAddr),
		fmt.Sprintf("SOFT_SERVE_GIT_PUBLIC_URL=%s", c.Git.PublicURL),
		fmt.Sprintf("SOFT_SERVE_GIT_MAX_TIMEOUT=%d", c.Git.MaxTimeout),
		fmt.Sprintf("SOFT_SERVE_GIT_IDLE_TIMEOUT=%d", c.Git.IdleTimeout),
		fmt.Sprintf("SOFT_SERVE_GIT_MAX_CONNECTIONS=%d", c.Git.MaxConnections),
		fmt.Sprintf("SOFT_SERVE_HTTP_ENABLED=%t", c.HTTP.Enabled),
		fmt.Sprintf("SOFT_SERVE_HTTP_LISTEN_ADDR=%s", c.HTTP.ListenAddr),
		fmt.Sprintf("SOFT_SERVE_HTTP_TLS_KEY_PATH=%s", c.HTTP.TLSKeyPath),
		fmt.Sprintf("SOFT_SERVE_HTTP_TLS_CERT_PATH=%s", c.HTTP.TLSCertPath),
		fmt.Sprintf("SOFT_SERVE_HTTP_PUBLIC_URL=%s", c.HTTP.PublicURL),
		fmt.Sprintf("SOFT_SERVE_HTTP_CORS_ALLOWED_HEADERS=%s", strings.Join(c.HTTP.CORS.AllowedHeaders, ",")),
		fmt.Sprintf("SOFT_SERVE_HTTP_CORS_ALLOWED_ORIGINS=%s", strings.Join(c.HTTP.CORS.AllowedOrigins, ",")),
		fmt.Sprintf("SOFT_SERVE_HTTP_CORS_ALLOWED_METHODS=%s", strings.Join(c.HTTP.CORS.AllowedMethods, ",")),
		fmt.Sprintf("SOFT_SERVE_STATS_ENABLED=%t", c.Stats.Enabled),
		fmt.Sprintf("SOFT_SERVE_STATS_LISTEN_ADDR=%s", c.Stats.ListenAddr),
		fmt.Sprintf("SOFT_SERVE_LOG_FORMAT=%s", c.Log.Format),
		fmt.Sprintf("SOFT_SERVE_LOG_TIME_FORMAT=%s", c.Log.TimeFormat),
		fmt.Sprintf("SOFT_SERVE_DB_DRIVER=%s", c.DB.Driver),
		fmt.Sprintf("SOFT_SERVE_DB_DATA_SOURCE=%s", c.DB.DataSource),
		fmt.Sprintf("SOFT_SERVE_DB_MAX_OPEN_CONNS=%d", c.DB.MaxOpenConns),
		fmt.Sprintf("SOFT_SERVE_DB_MAX_IDLE_CONNS=%d", c.DB.MaxIdleConns),
		fmt.Sprintf("SOFT_SERVE_DB_CONN_MAX_LIFETIME=%d", c.DB.ConnMaxLifetime),
		fmt.Sprintf("SOFT_SERVE_DB_CONN_MAX_IDLE_TIME=%d", c.DB.ConnMaxIdleTime),
		fmt.Sprintf("SOFT_SERVE_LFS_ENABLED=%t", c.LFS.Enabled),
		fmt.Sprintf("SOFT_SERVE_LFS_SSH_ENABLED=%t", c.LFS.SSHEnabled),
		fmt.Sprintf("SOFT_SERVE_JOBS_MIRROR_PULL=%s", c.Jobs.MirrorPull),
	}...)

	// AnonAccess and AllowKeyless are tri-state overrides: only emit them
	// when explicitly set, so a subprocess parsing these envs sees the same
	// "unset" state (rather than an empty string coercing to a zero value).
	if c.AnonAccess != nil {
		envs = append(envs, fmt.Sprintf("SOFT_SERVE_ANON_ACCESS=%s", c.AnonAccess.String()))
	}

	if c.AllowKeyless != nil {
		envs = append(envs, fmt.Sprintf("SOFT_SERVE_ALLOW_KEYLESS=%t", *c.AllowKeyless))
	}

	envs = append(envs, fmt.Sprintf("SOFT_SERVE_ADMIN_ONLY_CREATE=%t", c.AdminOnlyCreate))
	envs = append(envs, fmt.Sprintf("SOFT_SERVE_MAX_REPOS_PER_USER=%d", c.MaxReposPerUser))

	return envs
}

// IsDebug returns true if the server is running in debug mode.
func IsDebug() bool {
	debug, _ := strconv.ParseBool(os.Getenv("SOFT_SERVE_DEBUG"))
	return debug
}

// IsVerbose returns true if the server is running in verbose mode.
// Verbose mode is only enabled if debug mode is enabled.
func IsVerbose() bool {
	verbose, _ := strconv.ParseBool(os.Getenv("SOFT_SERVE_VERBOSE"))
	return IsDebug() && verbose
}

// parseFile parses the given file as a configuration file.
// The file must be in YAML format.
func parseFile(cfg *Config, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}

	defer f.Close() //nolint: errcheck
	if err := yaml.NewDecoder(f).Decode(cfg); err != nil {
		return fmt.Errorf("decode config: %w", err)
	}

	return cfg.Validate()
}

// ParseFile parses the config from the default file path.
// This also calls Validate() on the config.
func (c *Config) ParseFile() error {
	return parseFile(c, c.ConfigPath())
}

// parseEnv parses the environment variables as a configuration file.
func parseEnv(cfg *Config) error {
	// Initial admin keys from the config file and the environment are
	// merged, not overridden. Keep the file keys to add them back below.
	fileAdminKeys := slices.Clone(cfg.InitialAdminKeys)

	// Override with environment variables
	if err := env.ParseWithOptions(cfg, env.Options{
		Prefix: "SOFT_SERVE_",
	}); err != nil {
		return fmt.Errorf("parse environment variables: %w", err)
	}

	// The env parser leaves the list alone when the variable is unset and
	// replaces it otherwise. Either way, the env keys come first, followed
	// by each file key not already listed.
	cfg.InitialAdminKeys = mergeAdminKeys(cfg.InitialAdminKeys, fileAdminKeys)

	return cfg.Validate()
}

// mergeAdminKeys returns the entries of a followed by those of b, each
// listed once. Entries are compared after trimming surrounding space.
func mergeAdminKeys(a, b []string) []string {
	merged := make([]string, 0, len(a)+len(b))
	seen := make(map[string]struct{}, len(a)+len(b))
	for _, k := range slices.Concat(a, b) {
		key := strings.TrimSpace(k)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, k)
	}
	return merged
}

// ParseEnv parses the config from the environment variables.
// This also calls Validate() on the config.
func (c *Config) ParseEnv() error {
	return parseEnv(c)
}

// Parse parses the config from the default file path and environment variables.
// This also calls Validate() on the config.
func (c *Config) Parse() error {
	if err := c.ParseFile(); err != nil {
		return err
	}

	return c.ParseEnv()
}

// writeConfig writes the configuration to the given file.
func writeConfig(cfg *Config, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), os.ModePerm); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(newConfigFile(cfg)), 0o644) //nolint: errcheck, gosec
}

// WriteConfig writes the configuration to the default file.
func (c *Config) WriteConfig() error {
	return writeConfig(c, c.ConfigPath())
}

// DefaultDataPath returns the path to the data directory.
// It uses the SOFT_SERVE_DATA_PATH environment variable if set, otherwise it
// uses "data".
func DefaultDataPath() string {
	dp := os.Getenv("SOFT_SERVE_DATA_PATH")
	if dp == "" {
		dp = "data"
	}

	return dp
}

// ConfigPath returns the path to the config file.
func (c *Config) ConfigPath() string { //nolint:revive
	// If we have a custom config location set, then use that.
	if path := os.Getenv("SOFT_SERVE_CONFIG_LOCATION"); exist(path) {
		return path
	}

	// Otherwise, look in the data path.
	return filepath.Join(c.DataPath, "config.yaml")
}

func exist(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Exist returns true if the config file exists.
func (c *Config) Exist() bool {
	return exist(c.ConfigPath())
}

// DefaultConfig returns the default Config. All the path values are relative
// to the data directory.
// Use Validate() to validate the config and ensure absolute paths.
func DefaultConfig() *Config {
	return &Config{
		Name: "Soft Serve Plus",
		// DefaultRepo: "",
		DataPath: DefaultDataPath(),
		SSH: SSHConfig{
			Enabled:                     true,
			ListenAddr:                  ":23231",
			PublicURL:                   "ssh://localhost:23231",
			KeyPath:                     filepath.Join("ssh", "soft_serve_host_ed25519"),
			ClientKeyPath:               filepath.Join("ssh", "soft_serve_client_ed25519"),
			ClientStrictHostKeyChecking: StrictHostKeyCheckingAcceptNew,
			MaxTimeout:                  0,
			IdleTimeout:                 10 * 60, // 10 minutes
		},
		Git: GitConfig{
			Enabled:        true,
			ListenAddr:     ":9418",
			PublicURL:      "git://localhost",
			MaxTimeout:     0,
			IdleTimeout:    3,
			MaxConnections: 32,
		},
		HTTP: HTTPConfig{
			Enabled:    true,
			ListenAddr: ":23232",
			PublicURL:  "http://localhost:23232",
			CORS: CORSConfig{
				AllowedHeaders: []string{"Accept", "Accept-Language", "Content-Language", "Content-Type", "Origin", "X-Requested-With", "User-Agent", "Authorization", "Access-Control-Request-Method", "Access-Control-Allow-Origin"},
				AllowedMethods: []string{"GET", "HEAD", "POST", "PUT", "OPTIONS"},
				AllowedOrigins: []string{"http://localhost:23232"},
			},
		},
		Stats: StatsConfig{
			Enabled:    true,
			ListenAddr: "localhost:23233",
		},
		Log: LogConfig{
			Format:     "text",
			TimeFormat: time.DateTime,
		},
		DB: DBConfig{
			Driver: "sqlite",
			DataSource: "soft-serve.db" +
				"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)",
		},
		LFS: LFSConfig{
			Enabled:    true,
			SSHEnabled: false,
		},
		Jobs: JobsConfig{
			MirrorPull: "@every 10m",
		},
	}
}

// Validate validates the configuration.
// It updates the configuration with absolute paths.
func (c *Config) Validate() error {
	// The data path is handed to ssh inside a double-quoted option value,
	// where nothing escapes a quote or a newline.
	if strings.ContainsAny(c.DataPath, "\"\n\r") {
		return fmt.Errorf("data path must not contain quotes or newlines: %q", c.DataPath)
	}

	// Use absolute paths
	if !filepath.IsAbs(c.DataPath) {
		dp, err := filepath.Abs(c.DataPath)
		if err != nil {
			return err
		}
		c.DataPath = dp
	}

	c.SSH.PublicURL = strings.TrimSuffix(c.SSH.PublicURL, "/")
	c.HTTP.PublicURL = strings.TrimSuffix(c.HTTP.PublicURL, "/")

	if c.SSH.KeyPath != "" && !filepath.IsAbs(c.SSH.KeyPath) {
		c.SSH.KeyPath = filepath.Join(c.DataPath, c.SSH.KeyPath)
	}

	if c.SSH.ClientKeyPath != "" && !filepath.IsAbs(c.SSH.ClientKeyPath) {
		c.SSH.ClientKeyPath = filepath.Join(c.DataPath, c.SSH.ClientKeyPath)
	}

	for _, v := range []struct {
		name  string
		value int
	}{
		{"db.max_open_conns", c.DB.MaxOpenConns},
		{"db.max_idle_conns", c.DB.MaxIdleConns},
		{"db.conn_max_lifetime", c.DB.ConnMaxLifetime},
		{"db.conn_max_idle_time", c.DB.ConnMaxIdleTime},
	} {
		if v.value < 0 {
			return fmt.Errorf("%s: %d is negative; use 0 for the default", v.name, v.value)
		}
	}

	if c.MaxReposPerUser < 0 {
		return fmt.Errorf("max_repos_per_user: %d is negative; use 0 for no limit", c.MaxReposPerUser)
	}

	switch c.SSH.ClientStrictHostKeyChecking {
	case "":
		c.SSH.ClientStrictHostKeyChecking = StrictHostKeyCheckingAcceptNew
	case StrictHostKeyCheckingAcceptNew, StrictHostKeyCheckingYes:
	default:
		// "no" and "off" are not offered: they accept any host key, which
		// hands the client key's authentication to whoever answers.
		return fmt.Errorf("ssh.client_strict_host_key_checking: %q is not one of %q, %q",
			c.SSH.ClientStrictHostKeyChecking, StrictHostKeyCheckingAcceptNew, StrictHostKeyCheckingYes)
	}

	// Drop blanks so an unset env var does not read as one empty entry, then
	// parse once, rejecting anything unreadable now rather than at the first
	// import.
	c.SSH.ClientAllowedHosts = slices.DeleteFunc(slices.Clone(c.SSH.ClientAllowedHosts), func(s string) bool {
		return strings.TrimSpace(s) == ""
	})
	allowed, err := ssrf.ParseAllowedHosts(c.SSH.ClientAllowedHosts)
	if err != nil {
		return fmt.Errorf("ssh.client_allowed_hosts: %w", err)
	}
	c.SSH.allowedHosts = allowed

	if c.HTTP.TLSKeyPath != "" && !filepath.IsAbs(c.HTTP.TLSKeyPath) {
		c.HTTP.TLSKeyPath = filepath.Join(c.DataPath, c.HTTP.TLSKeyPath)
	}

	if c.HTTP.TLSCertPath != "" && !filepath.IsAbs(c.HTTP.TLSCertPath) {
		c.HTTP.TLSCertPath = filepath.Join(c.DataPath, c.HTTP.TLSCertPath)
	}

	if strings.HasPrefix(c.DB.Driver, "sqlite") && !filepath.IsAbs(c.DB.DataSource) {
		c.DB.DataSource = filepath.Join(c.DataPath, c.DB.DataSource)
	}

	// Validate keys
	pks := make([]string, 0)
	keys := parseAuthKeys(c.InitialAdminKeys)
	for _, key := range keys.pks {
		ak := sshutils.MarshalAuthorizedKey(key)
		pks = append(pks, ak)
	}
	c.unreadableAdminKeyFiles = addToSet(c.unreadableAdminKeyFiles, keys.unreadable)
	c.ignoredAdminKeys = addToSet(c.ignoredAdminKeys, keys.ignored)

	c.InitialAdminKeys = pks

	if !slices.Contains(c.HTTP.CORS.AllowedOrigins, c.HTTP.PublicURL) {
		c.HTTP.CORS.AllowedOrigins = append([]string{c.HTTP.PublicURL}, c.HTTP.CORS.AllowedOrigins...)
	}

	return nil
}

// authKeys is the result of parseAuthKeys.
type authKeys struct {
	pks []ssh.PublicKey
	// unreadable holds the entries that name a key file that could not be
	// read or holds no key, such as a secret that is not mounted yet. Such
	// an entry may stand for a key that is still wanted.
	unreadable []string
	// ignored holds the entries that are neither a key nor a file path,
	// such as a mistyped key. They cannot stand for a wanted key.
	ignored []string
}

// parseAuthKeys parses authorized keys from either file paths or string
// authorized_keys. Blank entries and entries starting with "#" are comments
// and are skipped.
func parseAuthKeys(aks []string) authKeys {
	var r authKeys
	exist := make(map[string]struct{}, 0)
	r.pks = make([]ssh.PublicKey, 0)
	for _, entry := range aks {
		key := strings.TrimSpace(entry)
		if key == "" || strings.HasPrefix(key, "#") {
			continue
		}
		if bts, err := os.ReadFile(key); err == nil {
			// key is a file in authorized_keys format: one key per line.
			var found bool
			var bad []string
			for _, line := range strings.Split(string(bts), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				pk, _, err := sshutils.ParseAuthorizedKey(line)
				if err != nil {
					bad = append(bad, line)
					continue
				}
				found = true
				id := string(pk.Marshal())
				if _, ok := exist[id]; !ok {
					r.pks = append(r.pks, pk)
					exist[id] = struct{}{}
				}
			}
			if !found {
				r.unreadable = append(r.unreadable, key)
				continue
			}
			// A file that holds keys was read; its other lines are
			// mistyped keys.
			r.ignored = append(r.ignored, bad...)
			continue
		}

		pk, _, err := sshutils.ParseAuthorizedKey(key)
		switch {
		case err == nil:
			// Compare the key itself, not its text: a key file, a comment
			// or extra spacing must not list the same key twice.
			id := string(pk.Marshal())
			if _, ok := exist[id]; !ok {
				r.pks = append(r.pks, pk)
				exist[id] = struct{}{}
			}
		case looksLikePath(key):
			r.unreadable = append(r.unreadable, strings.TrimSpace(entry))
		default:
			r.ignored = append(r.ignored, key)
		}
	}
	return r
}

// looksLikePath reports whether an entry that is not a key names a file. An
// authorized key always has a space between its type and its data, so an
// entry with a space is a malformed key; an entry without one is a path if
// it has a directory separator or a file extension.
func looksLikePath(s string) bool {
	if strings.ContainsFunc(s, unicode.IsSpace) {
		return false
	}
	return strings.ContainsAny(s, `/\`) || filepath.Ext(s) != ""
}

// addToSet adds items to set, creating it if needed.
func addToSet(set map[string]struct{}, items []string) map[string]struct{} {
	if len(items) == 0 {
		return set
	}
	if set == nil {
		set = make(map[string]struct{}, len(items))
	}
	for _, it := range items {
		set[it] = struct{}{}
	}
	return set
}

// AdminKeys returns the server admin keys.
func (c *Config) AdminKeys() []ssh.PublicKey {
	return parseAuthKeys(c.InitialAdminKeys).pks
}

// UnreadableAdminKeyFiles returns, sorted and without duplicates, the
// initial_admin_keys entries that name a key file that could not be read or
// holds no key, such as a secret that is not mounted yet. Such an entry may
// stand for a key that is still wanted. Paths are not key material.
func (c *Config) UnreadableAdminKeyFiles() []string {
	set := addToSet(maps.Clone(c.unreadableAdminKeyFiles), parseAuthKeys(c.InitialAdminKeys).unreadable)
	return slices.Sorted(maps.Keys(set))
}

// IgnoredAdminKeys returns how many distinct initial_admin_keys entries were
// ignored because they are neither a key nor a file path, such as a mistyped
// key. The entries are not returned, as they may hold key material.
func (c *Config) IgnoredAdminKeys() int {
	return len(addToSet(maps.Clone(c.ignoredAdminKeys), parseAuthKeys(c.InitialAdminKeys).ignored))
}

func init() {
	if ex, err := os.Executable(); err == nil {
		binPath = filepath.ToSlash(ex)
	}
}
