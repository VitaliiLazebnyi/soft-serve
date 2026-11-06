package backend

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	charmssh "charm.land/ssh"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
	"github.com/matryer/is"
	"golang.org/x/crypto/ssh"
)

// TestCreateRepositoryAnonymousOwner verifies that a repository created with
// a nil user (an anon-access/allow-keyless override) is owned by the
// lowest-ID admin rather than failing the NOT NULL repos.user_id constraint.
func TestCreateRepositoryAnonymousOwner(t *testing.T) {
	is := is.New(t)
	be, _ := newTestBackend(t)
	ctx := context.Background()

	admin, err := be.User(ctx, "admin")
	is.NoErr(err)

	repo, err := be.CreateRepository(ctx, "anon-repo", nil, proto.RepositoryOptions{})
	is.NoErr(err)
	is.Equal(repo.UserID(), admin.ID())
}

// TestDefaultAdminUserIDNoAdmin verifies that defaultAdminUserID surfaces an
// error rather than silently returning a zero user ID (which would violate
// the NOT NULL repos.user_id constraint) when the database has no admin.
func TestDefaultAdminUserIDNoAdmin(t *testing.T) {
	is := is.New(t)
	be, _ := newTestBackend(t)
	ctx := context.Background()

	err := be.db.TransactionContext(ctx, func(tx *db.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE users SET admin = false"); err != nil {
			return err
		}

		_, err := be.defaultAdminUserID(ctx, tx)
		return err
	})
	is.True(err != nil)
}

// TestValidateImportRemote verifies that import remotes pointing at private,
// internal, or non-network destinations are rejected, that the ssh policy
// maps to the right user-facing error, and that accepted remotes carry the
// git environment that keeps the validation honest.
func TestValidateImportRemote(t *testing.T) {
	allowed, err := ssrf.ParseAllowedHosts([]string{"git.internal.example", "10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	nobody := ssrf.GitRemotePolicy{}
	admin := ssrf.GitRemotePolicy{AllowAnySSHHost: true}
	listed := ssrf.GitRemotePolicy{AllowedSSHHosts: allowed}

	tests := []struct {
		name    string
		remote  string
		policy  ssrf.GitRemotePolicy
		wantErr error
	}{
		{"public https", "https://1.1.1.1/x.git", nobody, nil},
		{"public git", "git://1.1.1.1/x.git", nobody, nil},
		// With no allowlist, ssh is for admins only; the user sees an
		// authorization error, not a remote one.
		{"ssh admin", "ssh://git@10.0.0.1/x.git", admin, nil},
		{"ssh user", "ssh://git@10.0.0.1/x.git", nobody, proto.ErrUnauthorized},
		{"ssh allowed by name", "ssh://git@git.internal.example/x.git", listed, nil},
		{"ssh allowed by cidr", "git@10.1.1.1:x.git", listed, nil},
		{"ssh not on allowlist", "ssh://git@evil.example/x.git", listed, proto.ErrInvalidRemote},
		{"scp no user", "github.com:charmbracelet/soft-serve.git", admin, nil},
		{"scp ipv6", "git@[::1]:x.git", admin, nil},

		{"loopback", "http://127.0.0.1/x.git", nobody, proto.ErrInvalidRemote},
		{"localhost", "http://localhost:8080/x.git", nobody, proto.ErrInvalidRemote},
		{"private network", "http://10.0.0.1/x.git", nobody, proto.ErrInvalidRemote},
		{"cloud metadata", "http://169.254.169.254/latest/meta-data/", nobody, proto.ErrInvalidRemote},
		{"git scheme private", "git://10.0.0.1/x.git", nobody, proto.ErrInvalidRemote},
		{"octal loopback", "http://0177.0.0.1/x.git", nobody, proto.ErrInvalidRemote},
		{"local path", "/data/repos/secret.git", nobody, proto.ErrInvalidRemote},
		{"file scheme", "file:///etc/passwd", nobody, proto.ErrInvalidRemote},
		{"empty", "", nobody, proto.ErrInvalidRemote},
		// Regression: a URL Go cannot parse but curl can was classified as
		// SSH and skipped validation entirely.
		{"unparseable http to loopback", "http://a{b@127.0.0.1:8080/", nobody, proto.ErrInvalidRemote},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			is := is.New(t)
			v, err := validateImportRemote(context.Background(), tt.remote, tt.policy)

			if tt.wantErr != nil {
				is.True(errors.Is(err, tt.wantErr))
				return
			}

			is.NoErr(err)
			env := ssrf.GitEnv(v)
			// Redirect following must be off, or a public remote can hand
			// off to an internal one after validation has passed.
			is.True(slices.Contains(env, "GIT_CONFIG_VALUE_0=false"))
			is.True(slices.Contains(env, "GIT_CONFIG_KEY_0=http.followRedirects"))
		})
	}
}

// TestImportRepositoryCloneFailure verifies that a failed clone returns to the
// caller promptly with a fixed error and leaves nothing behind. Before the fix
// the caller blocked forever on a channel that was only written after a
// successful clone, leaking a goroutine per failed import.
func TestImportRepositoryCloneFailure(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh client not available")
	}

	is := is.New(t)
	be, _ := newTestBackend(t)
	ctx := context.Background()

	admin, err := be.User(ctx, "admin")
	is.NoErr(err)

	// An ssh remote passes validation, and nothing listens on port 1 of
	// loopback, so the clone is refused immediately without touching the
	// network.
	type result struct {
		repo proto.Repository
		err  error
	}
	resc := make(chan result, 1)
	go func() {
		r, err := be.ImportRepository(ctx, "broken", admin, "ssh://127.0.0.1:1/x.git", proto.RepositoryOptions{})
		resc <- result{r, err}
	}()

	select {
	case res := <-resc:
		is.True(res.repo == nil)
		is.True(errors.Is(res.err, proto.ErrImportFailed))
		// The clone's own message must not reach the caller.
		is.Equal(res.err.Error(), proto.ErrImportFailed.Error())
	case <-time.After(30 * time.Second):
		t.Fatal("ImportRepository did not return after a failed clone")
	}

	_, err = be.Repository(ctx, "broken")
	is.True(errors.Is(err, proto.ErrRepoNotFound))
	_, err = os.Stat(be.repoPath("broken"))
	is.True(errors.Is(err, os.ErrNotExist))
}

func TestRedactRemote(t *testing.T) {
	tests := map[string]string{
		"https://user:secret@example.com/x.git": "https://example.com/x.git",
		"https://TOKEN@example.com/x.git":       "https://example.com/x.git",
		"https://example.com/x.git?t=TOKEN":     "https://example.com/x.git",
		"https://example.com/x.git":             "https://example.com/x.git",
		"git@github.com:x/y.git":                "git@github.com:x/y.git",
		"ssh://git@10.0.0.1/x.git":              "ssh://10.0.0.1/x.git",
	}
	for in, want := range tests {
		if got := redactRemote(in); got != want {
			t.Errorf("redactRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestImportRepositorySSHRequiresAdmin verifies that with no ssh allowlist
// configured, only a server admin may import over ssh. The clone would
// present the server's client key to whatever host the remote names.
func TestImportRepositorySSHRequiresAdmin(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := context.Background()
	is.Equal(len(cfg.SSH.AllowedHosts()), 0)

	user, err := be.CreateUser(ctx, "tenant", proto.UserOptions{})
	is.NoErr(err)

	for _, remote := range []string{
		"ssh://git@10.0.0.1/x.git",
		"git+ssh://git@10.0.0.1/x.git",
		"git@10.0.0.1:x.git",
		"[git@10.0.0.1:2222]:x.git",
	} {
		_, err = be.ImportRepository(ctx, "tenant-ssh", user, remote, proto.RepositoryOptions{})
		is.True(errors.Is(err, proto.ErrUnauthorized)) // remote
	}

	_, err = be.ImportRepository(ctx, "tenant-ssh", nil, "ssh://git@10.0.0.1/x.git", proto.RepositoryOptions{})
	is.True(errors.Is(err, proto.ErrUnauthorized))

	// An ssh LFS endpoint is subject to the same policy as the remote.
	_, err = be.ImportRepository(ctx, "tenant-ssh", user, "https://1.1.1.1/x.git", proto.RepositoryOptions{LFSEndpoint: "ssh://git@10.0.0.1/x.git"})
	is.True(errors.Is(err, proto.ErrUnauthorized))

	// An allowlist entry that does not cover the host is refused for
	// everyone, admin included, with the remote error rather than an
	// authorization one.
	admin, err := be.User(ctx, "admin")
	is.NoErr(err)
	cfg.SSH.ClientAllowedHosts = []string{"git.internal.example"}
	is.NoErr(cfg.Validate())
	_, err = be.ImportRepository(ctx, "admin-ssh", admin, "ssh://git@10.0.0.1/x.git", proto.RepositoryOptions{})
	is.True(errors.Is(err, proto.ErrInvalidRemote))
	is.True(errors.Is(err, ssrf.ErrSSHHostNotAllowed))
	_, err = be.ImportRepository(ctx, "tenant-ssh", user, "ssh://git@10.0.0.1/x.git", proto.RepositoryOptions{})
	is.True(errors.Is(err, proto.ErrInvalidRemote))

	// Nothing was created along the way.
	_, err = be.Repository(ctx, "tenant-ssh")
	is.True(errors.Is(err, proto.ErrRepoNotFound))
	_, err = be.Repository(ctx, "admin-ssh")
	is.True(errors.Is(err, proto.ErrRepoNotFound))
}

// TestImportRepositorySSHAdminKey verifies that a caller whose session key is
// listed in initial_admin_keys may import over ssh with no allowlist, whether
// it has no account or a non-admin one, as the SSH layer's isServerAdmin
// treats it. The same caller without the key on the context, or with any
// other key, is still refused.
func TestImportRepositorySSHAdminKey(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh client not available")
	}

	for _, registered := range []bool{false, true} {
		name := "anonymous"
		if registered {
			name = "non-admin user"
		}
		t.Run(name, func(t *testing.T) {
			is := is.New(t)
			be, cfg := newTestBackend(t)
			ctx := backendTestContext(be, cfg)
			is.Equal(len(cfg.SSH.AllowedHosts()), 0)

			pub := newTestPublicKey(t)
			cfg.InitialAdminKeys = append(cfg.InitialAdminKeys, sshutils.MarshalAuthorizedKey(pub))

			var user proto.User
			if registered {
				var err error
				user, err = be.CreateUser(ctx, "alice", proto.UserOptions{PublicKeys: []ssh.PublicKey{pub}})
				is.NoErr(err)
				is.True(!user.IsAdmin())
			}

			// Nothing listens on loopback port 1, so a remote that passes
			// policy fails at the clone without touching the network.
			remote := "ssh://git@127.0.0.1:1/x.git"
			opts := proto.RepositoryOptions{}

			_, err := be.ImportRepository(ctx, "nokey", user, remote, opts)
			is.True(errors.Is(err, proto.ErrUnauthorized)) // no key on the context

			otherCtx := context.WithValue(ctx, charmssh.ContextKeyPublicKey, newTestPublicKey(t))
			_, err = be.ImportRepository(otherCtx, "otherkey", user, remote, opts)
			is.True(errors.Is(err, proto.ErrUnauthorized)) // not an admin key

			keyCtx := context.WithValue(ctx, charmssh.ContextKeyPublicKey, pub)
			_, err = be.ImportRepository(keyCtx, "adminkey", user, remote, opts)
			is.True(errors.Is(err, proto.ErrImportFailed)) // admin key passes policy

			// An ssh LFS endpoint is checked, before the clone, against the
			// same policy, so the admin key covers it as well.
			lfsOpts := proto.RepositoryOptions{LFSEndpoint: remote}
			_, err = be.ImportRepository(otherCtx, "otherkey-lfs", user, remote, lfsOpts)
			is.True(errors.Is(err, proto.ErrUnauthorized))
			_, err = be.ImportRepository(keyCtx, "adminkey-lfs", user, remote, lfsOpts)
			is.True(errors.Is(err, proto.ErrImportFailed))

			// A mirror is synced later by the mirror job with its owner's
			// stored privileges. An anonymous caller's mirror is owned by
			// the default admin, so the key suffices. A non-admin account's
			// mirror would never sync, so it is refused with the reason.
			mirrorOpts := proto.RepositoryOptions{Mirror: true}
			_, err = be.ImportRepository(keyCtx, "adminkey-mirror", user, remote, mirrorOpts)
			if registered {
				is.True(errors.Is(err, proto.ErrUnauthorized))
				is.True(errors.Is(err, errMirrorImportOwnerNotAdmin))
				is.True(errors.Is(err, errMirrorOwnerNotAdmin))
			} else {
				is.True(errors.Is(err, proto.ErrImportFailed))
			}
			mirrorOpts.LFSEndpoint = remote
			_, err = be.ImportRepository(keyCtx, "adminkey-mirror-lfs", user, remote, mirrorOpts)
			if registered {
				is.True(errors.Is(err, errMirrorOwnerNotAdmin))
			} else {
				is.True(errors.Is(err, proto.ErrImportFailed))
			}

			// A configured allowlist binds the admin key too. A host it
			// names is open to the mirror job as well, so a non-admin
			// account may mirror from it.
			cfg.SSH.ClientAllowedHosts = []string{"git.internal.example"}
			is.NoErr(cfg.Validate())
			_, err = be.ImportRepository(keyCtx, "adminkey-allowlist", user, remote, opts)
			is.True(errors.Is(err, proto.ErrInvalidRemote))
			is.True(errors.Is(err, ssrf.ErrSSHHostNotAllowed))
			cfg.SSH.ClientAllowedHosts = []string{"127.0.0.1:1"}
			is.NoErr(cfg.Validate())
			_, err = be.ImportRepository(keyCtx, "adminkey-allowed-mirror", user, remote, proto.RepositoryOptions{Mirror: true})
			is.True(errors.Is(err, proto.ErrImportFailed))
		})
	}
}

// TestSetMirrorChecksOwnerPolicy verifies that switching mirroring on is
// refused when the mirror job, which validates every remote with
// RemotePolicy for the owner, would skip the repository on every run, and
// that an LFS endpoint the job would skip is not a refusal. Such a repository arises when a non-admin account holding an
// admin key imports from ssh without --mirror.
func TestSetMirrorChecksOwnerPolicy(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	is.Equal(len(cfg.SSH.AllowedHosts()), 0)

	alice, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	is.NoErr(err)
	is.True(!alice.IsAdmin())
	admin, err := be.User(ctx, "admin")
	is.NoErr(err)

	// newRepo creates a repository owned by owner whose git config holds
	// the given remote and, if non-empty, LFS endpoint, as an import does.
	newRepo := func(name string, owner proto.User, remote, lfsURL string) {
		t.Helper()
		_, err := seedCreate(be, ctx, name, owner, proto.RepositoryOptions{})
		is.NoErr(err)
		r, err := git.Open(be.repoPath(name))
		is.NoErr(err)
		rcfg, err := r.Config()
		is.NoErr(err)
		rcfg.Section("remote").Subsection("origin").SetOption("url", remote)
		if lfsURL != "" {
			rcfg.Section("lfs").SetOption("url", lfsURL)
		}
		is.NoErr(r.SetConfig(rcfg))
	}
	isMirror := func(name string) bool {
		t.Helper()
		m, err := be.IsMirror(ctx, name)
		is.NoErr(err)
		return m
	}

	const sshRemote = "ssh://git@127.0.0.1:1/x.git"
	const httpsRemote = "https://1.1.1.1/x.git"

	// A non-admin owner's ssh remote would never sync, so it is refused,
	// and the flag and git config are left alone.
	newRepo("alice-ssh", alice, sshRemote, "")
	err = be.SetMirror(ctx, "alice-ssh", true)
	is.True(errors.Is(err, proto.ErrUnauthorized))
	is.True(errors.Is(err, errMirrorOwnerNotAdmin))
	is.True(!isMirror("alice-ssh"))
	r, err := git.Open(be.repoPath("alice-ssh"))
	is.NoErr(err)
	rcfg, err := r.Config()
	is.NoErr(err)
	is.Equal(rcfg.Section("remote").Subsection("origin").Option("mirror"), "")

	// An LFS endpoint that fails validation does not refuse the switch,
	// with LFS on or off: the job syncs the git data regardless and skips
	// only the LFS objects.
	newRepo("alice-lfs", alice, httpsRemote, sshRemote)
	is.True(cfg.LFS.Enabled)
	is.NoErr(be.SetMirror(ctx, "alice-lfs", true))
	is.True(isMirror("alice-lfs"))
	is.NoErr(be.SetMirror(ctx, "alice-lfs", false))
	cfg.LFS.Enabled = false
	is.NoErr(be.SetMirror(ctx, "alice-lfs", true))
	is.True(isMirror("alice-lfs"))
	cfg.LFS.Enabled = true

	// Switching mirroring off is never refused.
	is.NoErr(be.SetMirror(ctx, "alice-ssh", false))

	// A public https remote, and an admin owner's ssh remote, pass.
	newRepo("alice-https", alice, httpsRemote, "")
	is.NoErr(be.SetMirror(ctx, "alice-https", true))
	is.True(isMirror("alice-https"))
	newRepo("admin-ssh", admin, sshRemote, sshRemote)
	is.NoErr(be.SetMirror(ctx, "admin-ssh", true))
	is.True(isMirror("admin-ssh"))

	// An allowlist that names the host opens it to the job, and so to the
	// flag, for a non-admin owner. One that does not is refused with the
	// remote error, for an admin owner too.
	cfg.SSH.ClientAllowedHosts = []string{"127.0.0.1:1"}
	is.NoErr(cfg.Validate())
	is.NoErr(be.SetMirror(ctx, "alice-ssh", true))
	is.True(isMirror("alice-ssh"))
	newRepo("alice-other", alice, "ssh://git@10.0.0.1/x.git", "")
	err = be.SetMirror(ctx, "alice-other", true)
	is.True(errors.Is(err, proto.ErrInvalidRemote))
	is.True(errors.Is(err, ssrf.ErrSSHHostNotAllowed))
	is.True(!isMirror("alice-other"))
	is.NoErr(be.SetMirror(ctx, "admin-ssh", false))
	cfg.SSH.ClientAllowedHosts = []string{"git.internal.example"}
	is.NoErr(cfg.Validate())
	err = be.SetMirror(ctx, "admin-ssh", true)
	is.True(errors.Is(err, ssrf.ErrSSHHostNotAllowed))
	is.True(!isMirror("admin-ssh"))
}

// TestSetMirrorValidatesOutsideTransaction verifies that SetMirror resolves
// remotes with a deadline and before it opens a transaction, so a slow
// resolver cannot hold the database lock and stall other writers.
func TestSetMirrorValidatesOutsideTransaction(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)

	admin, err := be.User(ctx, "admin")
	is.NoErr(err)
	_, err = seedCreate(be, ctx, "slow", admin, proto.RepositoryOptions{})
	is.NoErr(err)
	r, err := git.Open(be.repoPath("slow"))
	is.NoErr(err)
	rcfg, err := r.Config()
	is.NoErr(err)
	rcfg.Section("remote").Subsection("origin").SetOption("url", "https://1.1.1.1/x.git")
	is.NoErr(r.SetConfig(rcfg))

	entered := make(chan bool, 1)
	release := make(chan struct{})
	prev := validateGitRemote
	t.Cleanup(func() { validateGitRemote = prev })
	validateGitRemote = func(ctx context.Context, remote string, policy ssrf.GitRemotePolicy) (ssrf.ValidatedGitRemote, error) {
		_, hasDeadline := ctx.Deadline()
		entered <- hasDeadline
		<-release
		return prev(ctx, remote, policy)
	}

	done := make(chan error, 1)
	go func() { done <- be.SetMirror(ctx, "slow", true) }()
	is.True(<-entered) // the lookup is bounded

	// While validation is stalled, another writer still commits.
	_, err = be.CreateUser(ctx, "bob", proto.UserOptions{})
	is.NoErr(err)

	close(release)
	is.NoErr(<-done)
	m, err := be.IsMirror(ctx, "slow")
	is.NoErr(err)
	is.True(m)
}

// TestImportRepositoryDisabled verifies that when the installed git is too
// old to honour the remote guards, or its version cannot be read, imports are
// refused before any remote is touched, and that a new enough git leaves the
// gate open.
func TestImportRepositoryDisabled(t *testing.T) {
	tests := []struct {
		name     string
		version  git.Version
		err      error
		disabled bool
	}{
		{"too old", git.Version{Major: 2, Minor: 30, Patch: 2}, nil, true},
		{"just below", git.Version{Major: 2, Minor: 32, Patch: 9}, nil, true},
		{"minimum", git.Version{Major: 2, Minor: 33}, nil, false},
		{"current", git.Version{Major: 2, Minor: 45}, nil, false},
		{"unreadable", git.Version{}, errors.New("exec: git: not found"), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			is := is.New(t)
			orig := binVersion
			binVersion = func() (git.Version, error) { return tt.version, tt.err }
			t.Cleanup(func() { binVersion = orig })

			be, _ := newTestBackend(t)
			ctx := context.Background()
			admin, err := be.User(ctx, "admin")
			is.NoErr(err)

			reason := be.ImportsDisabledReason()
			is.Equal(reason != "", tt.disabled)
			if !tt.disabled {
				return
			}

			_, err = be.ImportRepository(ctx, "gated", admin, "https://1.1.1.1/x.git", proto.RepositoryOptions{})
			is.True(errors.Is(err, proto.ErrImportsDisabled))
			// The reason is for the operator, not the user.
			is.Equal(err.Error(), proto.ErrImportsDisabled.Error())
			_, err = be.Repository(ctx, "gated")
			is.True(errors.Is(err, proto.ErrRepoNotFound))
		})
	}
}

// TestImportUnsupportedLFSEndpointRedacted verifies that an import whose LFS
// endpoint has no client (ssh) logs that endpoint without its credentials.
func TestImportUnsupportedLFSEndpointRedacted(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	is := is.New(t)
	var logs syncBuffer
	be, _ := newTestBackendWithLog(t, &logs)
	ctx := context.Background()

	admin, err := be.User(ctx, "admin")
	is.NoErr(err)

	src := t.TempDir()
	is.NoErr(exec.Command("git", "init", "--bare", "-q", src).Run())

	const endpoint = "ssh://TOKEN@example.com/x.git"
	ep, err := lfs.NewEndpoint(endpoint)
	is.NoErr(err)
	is.True(lfs.NewClient(ep) == nil) // the path under test

	imp := importSpec{
		name:        "lfs-ssh",
		path:        be.repoPath("lfs-ssh"),
		remote:      src,
		logRemote:   src,
		lfsEndpoint: endpoint,
		lfs:         ep,
	}
	is.NoErr(claimRepoPath(imp.path))
	r, err := be.importRepository(ctx, admin, imp)
	is.NoErr(err)
	is.True(r != nil)

	out := logs.String()
	is.True(strings.Contains(out, "unsupported endpoint"))
	if strings.Contains(out, "TOKEN") {
		t.Errorf("logs contain the endpoint credentials:\n%s", out)
	}
}
