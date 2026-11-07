package config

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
	"unicode/utf8"
)

var configFileTmpl = template.Must(template.New("config").Funcs(template.FuncMap{"q": yamlQuote}).Parse(`# Soft Serve Plus Server configurations

# The name of the server.
# This is the name that will be displayed in the UI.
name: {{ q .Name }}

# Logging configuration.
log:
  # Log format to use. Valid values are "json", "logfmt", and "text".
  format: {{ q .Log.Format }}
  # Time format for the log "timestamp" field.
  # Should be described in Golang's time format.
  time_format: {{ q .Log.TimeFormat }}
  # Path to the log file. Leave empty to write to stderr.
  #path: {{ q .Log.Path }}

# The SSH server configuration.
ssh:
  # Enable SSH.
  enabled: {{ .SSH.Enabled }}

  # The address on which the SSH server will listen.
  listen_addr: {{ q .SSH.ListenAddr }}

  # The public URL of the SSH server.
  # This is the address that will be used to clone repositories.
  public_url: {{ q .SSH.PublicURL }}

  # The path to the SSH server's private key.
  key_path: {{ q .SSH.KeyPath }}

  # The path to the server's client private key. This key will be used to
  # authenticate the server to make git requests to ssh remotes.
  client_key_path: {{ q .SSH.ClientKeyPath }}

  # How the server's ssh client verifies host keys of ssh remotes it imports
  # or mirrors from, checked against <data_path>/ssh/known_hosts.
  # "accept-new" records a host key on first contact and refuses a changed
  # one. "yes" refuses any host that is not already in known_hosts; use it
  # with a pre-seeded known_hosts file. The client always runs with
  # BatchMode=yes, so it fails fast instead of prompting for a passphrase.
  client_strict_host_key_checking: {{ q .SSH.ClientStrictHostKeyChecking }}

  # The ssh hosts that repositories may be imported from or mirrored. The
  # client key above is presented to every host a user imports from, so list
  # only hosts you trust that key with. Entries are hostnames, IP addresses,
  # or CIDR ranges, each with an optional port. When the list is empty, only
  # server admins may import from or mirror ssh remotes. Once the list is
  # non-empty it binds admins too: they may then reach only the hosts it
  # names. A mirror is synced with its owner's privileges, so mirroring
  # from ssh needs an owner with the admin flag or a host on this list.
  # CIDR entries are checked against DNS only at import or sync time and ssh
  # resolves the name again, so prefer hostnames for anything you do not
  # control. The list is matched against the name given to ssh; your
  # ssh_config still applies to it.
  client_allowed_hosts:
{{- range .SSH.ClientAllowedHosts }}
    - {{ q . }}
{{- else }} []
    # - "git.internal.example"
    # - "gitlab.corp:2222"
    # - "10.0.0.0/8"
{{- end }}

  # The maximum number of seconds a connection can take.
  # A value of 0 means no timeout.
  max_timeout: {{ .SSH.MaxTimeout }}

  # The number of seconds a connection can be idle before it is closed.
  # A value of 0 means no timeout.
  idle_timeout: {{ .SSH.IdleTimeout }}

  # The key exchange algorithms, ciphers and MACs the SSH server offers, in
  # order of preference. When a list is empty, the server offers every
  # algorithm golang.org/x/crypto/ssh considers secure, which leaves out the
  # SHA-1 based diffie-hellman-group14-sha1, diffie-hellman-group1-sha1,
  # diffie-hellman-group-exchange-sha1, hmac-sha1 and hmac-sha1-96. Names
  # x/crypto does not implement are rejected at startup. A list replaces the
  # default, so to let an old client that only speaks SHA-1 connect, copy the
  # full default list and append the legacy algorithm. Listing fewer would
  # drop the rest, such as the post-quantum mlkem768x25519-sha256:
  #   kex_algorithms:
  #     - "mlkem768x25519-sha256"
  #     - "curve25519-sha256"
  #     - "ecdh-sha2-nistp256"
  #     - "ecdh-sha2-nistp384"
  #     - "ecdh-sha2-nistp521"
  #     - "diffie-hellman-group14-sha256"
  #     - "diffie-hellman-group16-sha512"
  #     - "diffie-hellman-group-exchange-sha256"
  #     - "diffie-hellman-group14-sha1" # legacy, appended
  #   macs:
  #     - "hmac-sha2-256-etm@openssh.com"
  #     - "hmac-sha2-512-etm@openssh.com"
  #     - "hmac-sha2-256"
  #     - "hmac-sha2-512"
  #     - "hmac-sha1" # legacy, appended
  kex_algorithms:
{{- range .SSH.KexAlgorithms }}
    - {{ q . }}
{{- else }} []
{{- end }}
  ciphers:
{{- range .SSH.Ciphers }}
    - {{ q . }}
{{- else }} []
{{- end }}
  macs:
{{- range .SSH.MACs }}
    - {{ q . }}
{{- else }} []
{{- end }}

# The Git daemon configuration.
git:
  # Enable the Git daemon.
  enabled: {{ .Git.Enabled }}

  # The address on which the Git daemon will listen.
  listen_addr: {{ q .Git.ListenAddr }}

  # The public URL of the Git daemon server.
  # This is the address that will be used to clone repositories.
  public_url: {{ q .Git.PublicURL }}

  # The maximum number of seconds a connection can take.
  # A value of 0 means no timeout.
  max_timeout: {{ .Git.MaxTimeout }}

  # The number of seconds a connection can be idle before it is closed.
  idle_timeout: {{ .Git.IdleTimeout }}

  # The maximum number of concurrent connections.
  max_connections: {{ .Git.MaxConnections }}

# The HTTP server configuration.
http:
  # Enable the HTTP server.
  enabled: {{ .HTTP.Enabled }}

  # The address on which the HTTP server will listen.
  listen_addr: {{ q .HTTP.ListenAddr }}

  # The path to the TLS private key.
  tls_key_path: {{ q .HTTP.TLSKeyPath }}

  # The path to the TLS certificate.
  tls_cert_path: {{ q .HTTP.TLSCertPath }}

  # The public URL of the HTTP server.
  # This is the address that will be used to clone repositories.
  # Make sure to use https:// if you are using TLS.
  public_url: {{ q .HTTP.PublicURL }}

  # The cross-origin request security options
  cors:
    # The allowed cross-origin headers
    allowed_headers:
       - "Accept"
       - "Accept-Language"
       - "Content-Language"
       - "Content-Type"
       - "Origin"
       - "X-Requested-With"
       - "User-Agent"
       - "Authorization"
       - "Access-Control-Request-Method"
       - "Access-Control-Allow-Origin"
    # The allowed cross-origin URLs
    allowed_origins:
       - {{ q .HTTP.PublicURL }} # always allowed
       # - "https://example.com"
    # The allowed cross-origin methods
    allowed_methods:
       - "GET"
       - "HEAD"
       - "POST"
       - "PUT"
       - "OPTIONS"

# The stats server configuration.
stats:
  # Enable the stats server.
  enabled: {{ .Stats.Enabled }}

  # The address on which the stats server will listen.
  listen_addr: {{ q .Stats.ListenAddr }}

# The database configuration.
db:
  # The database driver to use.
  # Valid values are "sqlite" and "postgres".
  driver: {{ q .DB.Driver }}
  # The database data source name.
  # This is driver specific and can be a file path or connection string.
  # Make sure foreign key support is enabled when using SQLite.
  data_source: {{ q .DB.DataSource }}

# Git LFS configuration.
lfs:
  # Enable Git LFS.
  enabled: {{ .LFS.Enabled }}
  # Enable Git SSH transfer.
  ssh_enabled: {{ .LFS.SSHEnabled }}

# Cron job configuration
jobs:
  mirror_pull: {{ q .Jobs.MirrorPull }}

# Repository webhook configuration.
webhook:
  # Private and internal destinations webhooks may be sent to. Webhooks to
  # loopback, private networks and other internal addresses are refused
  # unless listed here. Entries are hostnames, IP addresses, or CIDR ranges,
  # each with an optional port (a URL without a port uses 80 for http and
  # 443 for https). An IP or CIDR entry allows the addresses it covers,
  # whatever hostname the webhook URL names. A hostname entry allows that
  # name to resolve to loopback, RFC1918, CGNAT or IPv6 unique local
  # addresses, and the name is resolved again for every delivery, so list
  # only names whose DNS you trust. Link-local and cloud metadata addresses
  # (such as 169.254.169.254) are reachable only through an IP or CIDR entry
  # inside their own range; a broad range such as 0.0.0.0/0 does not open
  # them. The list is checked when a webhook is created or updated, and
  # again on every delivery, so existing webhooks follow the current list.
  allowed_hosts:
{{- range .Webhook.AllowedHosts }}
    - {{ q . }}
{{- else }} []
    # - "jenkins.internal.example"
    # - "10.0.0.0/8"
    # - "127.0.0.1:8080"
{{- end }}

# Additional admin keys.
#initial_admin_keys:
#  - "ssh-rsa AAAAB3NzaC1yc2..."

# Only server admins (admin accounts or initial_admin_keys) may create
# repositories. Registered non-admins and anonymous users cannot create or
# import. Renaming counts as creating the new name, so repository owners and
# admin-access collaborators can no longer rename their repositories.
# Anonymous access to a name that does not exist yet is capped at read-only
# whatever anon_access says. Access to existing repositories is not affected.
# Defaults to false.
#admin_only_create: false

# Maximum number of repositories each user may create (repo create, repo
# import and push-to-create). Server admins are exempt, and repositories
# created without an account all count together as one user. A user at the
# limit is refused; existing repositories are kept. 0 means no limit.
#max_repos_per_user: 0

# Name of a repository to create automatically on boot if it doesn't already
# exist. Empty disables this behavior.
#default_repo: ""
`))

// yamlQuote renders s as a YAML double-quoted scalar that reads back as s.
// Backslashes and quotes are escaped, so a Windows path such as
// C:\data\soft-serve.db is not read as escape sequences, and characters YAML
// does not allow in a document, or would fold, are written as \u escapes.
func yamlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '"':
			b.WriteString(`\"`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20, r == 0x7f, r >= 0x80 && r < 0xa0, r == 0x2028, r == 0x2029, r == 0xfeff, r == 0xfffe, r == 0xffff, r == utf8.RuneError:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func newConfigFile(cfg *Config) string {
	var b bytes.Buffer
	configFileTmpl.Execute(&b, cfg) //nolint: errcheck
	return b.String()
}
