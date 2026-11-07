package backend

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/hooks"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	gitcfg "github.com/go-git/go-git/v5/plumbing/format/config"
)

// RemotePolicy returns the policy for git remotes supplied on behalf of user:
// the operator's ssh allowlist, and, for server admins, leave to pick any ssh
// host when no allowlist is configured. A nil user gets neither.
//
// Only the user's stored admin flag counts. It is meant for callers with no
// live session, such as the mirror job acting for a repository's owner. A
// caller acting for a live session must also honour an admin key on the
// session context (see isServerAdmin); ImportRepository does so through
// importRemotePolicy.
func RemotePolicy(cfg *config.Config, user proto.User) ssrf.GitRemotePolicy {
	return ssrf.GitRemotePolicy{
		AllowedSSHHosts: cfg.SSH.AllowedHosts(),
		AllowAnySSHHost: user != nil && user.IsAdmin(),
	}
}

// errMirrorOwnerNotAdmin explains a refused ssh mirror, at import or when
// mirroring is switched on later. It wraps proto.ErrUnauthorized, so callers
// that test for that still match. The allowlist is offered with its caveat:
// once ssh.client_allowed_hosts is non-empty it binds admins too, so adding
// one host narrows every admin's ssh reach to the list.
var errMirrorOwnerNotAdmin = fmt.Errorf("%w: an ssh mirror is synced with its owner's privileges, and the owner is not an admin; make the owner an admin, or list the host in ssh.client_allowed_hosts (a non-empty list also limits admins to the hosts it names)", proto.ErrUnauthorized)

// errMirrorImportOwnerNotAdmin is errMirrorOwnerNotAdmin at import time,
// where a plain import is a third way out.
var errMirrorImportOwnerNotAdmin = fmt.Errorf("%w, or import without --mirror", errMirrorOwnerNotAdmin)

// importRemotePolicy returns the policy for the remote and LFS endpoint that
// user, acting live on ctx, supplies to ImportRepository.
//
// The caller's own privilege counts: the stored admin flag or an admin key
// on the session (see isServerAdmin). A mirror is also synced later by the
// mirror job, which has no session and re-validates every remote with
// RemotePolicy for the repository's owner. So for a mirror the leave to use
// any ssh host is granted only when that later check will grant it too:
// the owner is the user when there is one, and the default admin otherwise
// (see CreateRepository). Without this, a non-admin account holding an
// admin key could create a mirror that the job then skips on every run.
// mirrorRestricted reports whether the mirror rule withdrew a leave the
// caller would otherwise have had.
func (d *Backend) importRemotePolicy(ctx context.Context, user proto.User, mirror bool) (policy ssrf.GitRemotePolicy, mirrorRestricted bool) {
	policy = RemotePolicy(d.cfg, user)
	live := d.isServerAdmin(ctx, user)
	policy.AllowAnySSHHost = live
	if mirror && user != nil && !user.IsAdmin() {
		policy.AllowAnySSHHost = false
		mirrorRestricted = live
	}
	return policy, mirrorRestricted
}

// validateImportRemote validates a user-supplied remote and maps the outcome
// to the errors the user sees: refused for want of privilege, or not a
// usable remote. Render the result with RemoteGitEnv and apply it to the
// clone subprocess, or the validation does not hold.
func validateImportRemote(ctx context.Context, remote string, policy ssrf.GitRemotePolicy) (ssrf.ValidatedGitRemote, error) {
	v, err := validateGitRemote(ctx, remote, policy)
	switch {
	case errors.Is(err, ssrf.ErrSSHRequiresAdmin):
		return ssrf.ValidatedGitRemote{}, proto.ErrUnauthorized
	case err != nil:
		return ssrf.ValidatedGitRemote{}, fmt.Errorf("%w: %w", proto.ErrInvalidRemote, err)
	}
	return v, nil
}

// validateGitRemote is ssrf.ValidateGitRemoteContext. Tests replace it to
// stand in for a slow resolver.
var validateGitRemote = ssrf.ValidateGitRemoteContext

// redactRemote strips any user info and query from a remote URL so it can be
// logged: either can hold a token. scp-style remotes pass through unchanged.
func redactRemote(remote string) string {
	return utils.RedactURL(remote)
}

// checkCanCreate refuses the creation of a new repository name by user when
// AdminOnlyCreate is set and the caller is not a server admin (see
// isServerAdmin). It is the backend's own guard, so a caller that skips the
// SSH and web access checks still cannot create. A nil user without an admin
// key is anonymous and is refused the same way.
func (d *Backend) checkCanCreate(ctx context.Context, user proto.User) error {
	if !d.cfg.AdminOnlyCreate {
		return nil
	}
	if d.isServerAdmin(ctx, user) {
		return nil
	}
	return proto.ErrUnauthorized
}

// repoLimited reports whether max_repos_per_user applies to user creating
// a repository: a limit is set and the caller is not a server admin (see
// isServerAdmin). Decide it on the caller's own context, which carries the
// session's public key.
func (d *Backend) repoLimited(ctx context.Context, user proto.User) bool {
	return d.cfg.MaxReposPerUser > 0 && !d.isServerAdmin(ctx, user)
}

// countRepos returns how many repositories user has created. Repositories
// created without an account (a nil user) count together as one user's.
func (d *Backend) countRepos(ctx context.Context, h db.Handler, user proto.User) (int64, error) {
	if user == nil || user.ID() == 0 {
		return d.store.CountAnonymousRepos(ctx, h)
	}
	return d.store.CountUserRepos(ctx, h, user.ID())
}

// errRepoLimit is the error for user being at max_repos_per_user.
func (d *Backend) errRepoLimit(user proto.User) error {
	if user == nil || user.ID() == 0 {
		return fmt.Errorf("%w: anonymous users may create %d repositories in all; delete one or ask an admin", proto.ErrRepoLimitReached, d.cfg.MaxReposPerUser)
	}
	return fmt.Errorf("%w: each user may create %d repositories; delete one or ask an admin", proto.ErrRepoLimitReached, d.cfg.MaxReposPerUser)
}

// CreateRepository creates a new repository.
//
// It implements backend.Backend.
func (d *Backend) CreateRepository(ctx context.Context, name string, user proto.User, opts proto.RepositoryOptions) (proto.Repository, error) {
	if err := d.checkCanCreate(ctx, user); err != nil {
		return nil, err
	}
	return d.createRepository(ctx, name, user, opts, d.repoLimited(ctx, user))
}

// CreateSystemRepository creates a repository on the server's own behalf,
// attributed to user. It is not subject to AdminOnlyCreate or
// MaxReposPerUser, so it is only for internal bootstrap (such as default_repo
// on boot) and must never be reached from a user request.
func (d *Backend) CreateSystemRepository(ctx context.Context, name string, user proto.User, opts proto.RepositoryOptions) (proto.Repository, error) {
	return d.createRepository(ctx, name, user, opts, false)
}

// createRepository creates an empty repository. limited says whether
// max_repos_per_user applies (see repoLimited).
func (d *Backend) createRepository(ctx context.Context, name string, user proto.User, opts proto.RepositoryOptions, limited bool) (proto.Repository, error) {
	name = utils.SanitizeRepo(name)
	if err := utils.ValidateRepo(name); err != nil {
		return nil, err
	}

	rp := d.repoPath(name)
	if err := claimRepoPath(rp); err != nil {
		return nil, err
	}

	if _, err := git.Init(rp, true); err != nil {
		if !errors.Is(err, git.ErrNotAGitRepository) {
			// openRepoError logs the other case.
			d.logger.Debug("failed to create repository", "err", err)
		}
		os.RemoveAll(rp) //nolint:errcheck
		return nil, openRepoError(d.logger, err, rp)
	}

	return d.registerRepository(ctx, name, user, opts, limited)
}

// claimRepoPath creates the repository directory, failing if anything already
// occupies it. The filesystem decides what "already" means, which is what
// makes this safe on case- and normalization-insensitive volumes.
func claimRepoPath(rp string) error {
	if err := os.MkdirAll(filepath.Dir(rp), os.ModePerm); err != nil {
		return err
	}
	if err := os.Mkdir(rp, os.ModePerm); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return proto.ErrRepoExist
		}
		return err
	}
	return nil
}

// registerRepository records a bare repository that already exists at the
// name's path and finishes setting it up. The directory is removed on failure.
// When limited, max_repos_per_user is checked in the same transaction that
// records the repository.
func (d *Backend) registerRepository(ctx context.Context, name string, user proto.User, opts proto.RepositoryOptions, limited bool) (proto.Repository, error) {
	rp := d.repoPath(name)

	var userID int64
	if user != nil {
		userID = user.ID()
	}
	anonymous := userID == 0

	if err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		if anonymous {
			// Anonymous callers (e.g. an anon-access/allow-keyless
			// override) have no user of their own, but repos.user_id is
			// NOT NULL. Fall back to the default admin as owner, the same
			// "ownerless repos belong to the first admin" idiom already
			// used when migrating repos from the pre-user schema (see
			// 0001_create_tables.go).
			adminID, err := d.defaultAdminUserID(ctx, tx)
			if err != nil {
				return err
			}
			userID = adminID
		}

		if err := d.store.CreateRepo(
			ctx,
			tx,
			name,
			userID,
			opts.ProjectName,
			opts.Description,
			opts.Private,
			opts.Hidden,
			opts.Mirror,
			anonymous,
		); err != nil {
			return err
		}

		// The limit is checked after the insert, in the same transaction,
		// which rolls the insert back if the creator is over it. As with the
		// webhook cap (see CreateWebhook), inserting first makes concurrent
		// SQLite writers wait for each other, and on Postgres the owner's
		// row is locked before counting, so creates racing at the limit
		// take turns.
		if limited {
			if err := d.store.LockUserRepos(ctx, tx, userID); err != nil {
				return err
			}
			n, err := d.countRepos(ctx, tx, user)
			if err != nil {
				return err
			}
			if n > int64(d.cfg.MaxReposPerUser) {
				return d.errRepoLimit(user)
			}
		}

		if err := os.WriteFile(filepath.Join(rp, "description"), []byte(opts.Description), fs.ModePerm); err != nil {
			d.logger.Error("failed to write description", "repo", name, "err", err)
			return err
		}

		if !opts.Private {
			if err := os.WriteFile(filepath.Join(rp, "git-daemon-export-ok"), []byte{}, fs.ModePerm); err != nil {
				d.logger.Error("failed to write git-daemon-export-ok", "repo", name, "err", err)
				return err
			}
		}

		return hooks.GenerateHooks(ctx, d.cfg, name)
	}); err != nil {
		d.logger.Debug("failed to create repository in database", "err", err)
		os.RemoveAll(rp) //nolint:errcheck
		err = db.WrapError(err)
		if errors.Is(err, db.ErrDuplicateKey) {
			return nil, proto.ErrRepoExist
		}

		return nil, err
	}

	return d.Repository(ctx, name)
}

// defaultAdminUserID returns the ID of the lowest-ID admin user, used to
// own repositories created by callers with no user of their own (anonymous
// access via an anon-access/allow-keyless override). The migration that
// seeds the initial database always creates an admin user, so this should
// never fail to find one in practice.
func (d *Backend) defaultAdminUserID(ctx context.Context, tx *db.Tx) (int64, error) {
	users, err := d.store.GetAllUsers(ctx, tx)
	if err != nil {
		return 0, err
	}

	var adminID int64
	for _, u := range users {
		if u.Admin && (adminID == 0 || u.ID < adminID) {
			adminID = u.ID
		}
	}

	if adminID == 0 {
		return 0, errors.New("no admin user found to own anonymous repository")
	}

	return adminID, nil
}

// ImportRepository imports a repository from remote.
func (d *Backend) ImportRepository(ctx context.Context, name string, user proto.User, remote string, opts proto.RepositoryOptions) (proto.Repository, error) {
	name = utils.SanitizeRepo(name)
	if err := utils.ValidateRepo(name); err != nil {
		return nil, err
	}

	if err := d.checkCanCreate(ctx, user); err != nil {
		return nil, err
	}

	// Refuse at the limit before cloning anything. registerRepository
	// checks again when it records the repository.
	limited := d.repoLimited(ctx, user)
	if limited {
		n, err := d.countRepos(ctx, d.db, user)
		if err != nil {
			return nil, err
		}
		if n >= int64(d.cfg.MaxReposPerUser) {
			return nil, d.errRepoLimit(user)
		}
	}

	if reason := d.ImportsDisabledReason(); reason != "" {
		d.logger.Warn("refusing import", "name", name, "reason", reason)
		return nil, proto.ErrImportsDisabled
	}

	remote = utils.Sanitize(remote)
	policy, mirrorRestricted := d.importRemotePolicy(ctx, user, opts.Mirror)
	// checkRemote validates one user-supplied remote, explaining a refusal
	// that only the mirror rule caused.
	checkRemote := func(r string) (ssrf.ValidatedGitRemote, error) {
		v, err := validateImportRemote(ctx, r, policy)
		if mirrorRestricted && errors.Is(err, proto.ErrUnauthorized) {
			d.logger.Warn("refusing ssh mirror import: owner is not an admin, so the mirror job could not sync it",
				"name", name, "remote", redactRemote(r), "user", user.Username())
			return v, errMirrorImportOwnerNotAdmin
		}
		return v, err
	}
	v, err := checkRemote(remote)
	if err != nil {
		return nil, err
	}

	imp := importSpec{
		name:      name,
		path:      d.repoPath(name),
		remote:    remote,
		logRemote: redactRemote(remote),
		env:       RemoteGitEnv(d.cfg, v),
		opts:      opts,
		limited:   limited,
	}

	// LFS objects are fetched only when asked for (--lfs or --lfs-endpoint);
	// a failure there then fails the import, since the user asked for it,
	// except for objects the remote doesn't have (see fetchImportLFSObjects).
	// The endpoint is resolved before anything is created, so a remote that
	// has no usable one (scp syntax, IPv6 literals) imports without LFS
	// instead of being cloned and then torn down.
	switch {
	case opts.LFSEndpoint != "":
		// A user-supplied endpoint is persisted to the repo config, where
		// the mirror job later reads it, so it obeys the same validation
		// and ssh policy as the remote itself.
		if _, err := checkRemote(opts.LFSEndpoint); err != nil {
			return nil, err
		}
		imp.lfs, err = lfs.NewEndpoint(opts.LFSEndpoint)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", proto.ErrInvalidRemote, err)
		}
		imp.lfsEndpoint = opts.LFSEndpoint
	case opts.LFS:
		if ep, err := lfs.NewEndpoint(remote); err == nil {
			imp.lfsEndpoint, imp.lfs = remote, ep
		}
	}

	// Claiming the directory is what serializes concurrent imports of one
	// name: the second one fails here with ErrRepoExist.
	if err := claimRepoPath(imp.path); err != nil {
		return nil, err
	}

	d.logger.Info("importing repository", "name", name, "remote", imp.logRemote, "path", imp.path)

	// The import runs on the backend's context rather than the caller's so a
	// dropped session does not leave a half-cloned repository behind.
	importCtx := proto.WithUserContext(d.ctx, user)
	r, err := d.importRepository(importCtx, user, imp)
	if err != nil {
		// A concurrent create can take the last slot while the clone runs.
		if errors.Is(err, proto.ErrRepoLimitReached) {
			return nil, err
		}
		// The real error is logged where it happened. The caller gets a
		// fixed message so a failed import cannot be used to probe hosts
		// and ports.
		return nil, proto.ErrImportFailed
	}

	return r, nil
}

// importSpec is a validated import, ready to run.
type importSpec struct {
	name, path        string
	remote, logRemote string
	// env is the git environment the clone must run with; see RemoteGitEnv.
	env []string
	// lfsEndpoint and lfs are set only when LFS objects are to be fetched.
	lfsEndpoint string
	lfs         lfs.Endpoint
	opts        proto.RepositoryOptions
	// limited says whether max_repos_per_user applies (see repoLimited).
	limited bool
}

// importRepository clones the remote into the claimed directory, registers
// the repository, and fetches LFS objects if asked. On failure nothing is
// left behind.
func (d *Backend) importRepository(ctx context.Context, user proto.User, imp importSpec) (_ proto.Repository, err error) {
	copts := git.CloneOptions{
		Bare:   true,
		Mirror: imp.opts.Mirror,
		Quiet:  true,
		CommandOptions: git.CommandOptions{
			Timeout: -1,
			Context: ctx,
			Envs:    imp.env,
		},
	}

	if err := git.Clone(imp.remote, imp.path, copts); err != nil {
		d.logger.Error("failed to clone repository", "err", err, "mirror", imp.opts.Mirror, "remote", imp.logRemote, "path", imp.path)
		if rerr := os.RemoveAll(imp.path); rerr != nil {
			err = errors.Join(err, rerr)
		}
		return nil, err
	}

	r, err := d.registerRepository(ctx, imp.name, user, imp.opts, imp.limited)
	if err != nil {
		d.logger.Error("failed to create repository", "err", err, "name", imp.name)
		return nil, err
	}

	defer func() {
		if err != nil {
			if rerr := d.DeleteRepository(ctx, imp.name); rerr != nil {
				d.logger.Error("failed to delete repository", "err", rerr, "name", imp.name)
			}
		}
	}()

	if imp.lfs == nil {
		return r, nil
	}

	rr, err := r.Open()
	if err != nil {
		d.logger.Error("failed to open repository", "err", err, "path", imp.path)
		return nil, err
	}

	rcfg, err := rr.Config()
	if err != nil {
		d.logger.Error("failed to get repository config", "err", err, "path", imp.path)
		return nil, err
	}

	rcfg.Section("lfs").SetOption("url", imp.lfsEndpoint)

	if err := rr.SetConfig(rcfg); err != nil {
		d.logger.Error("failed to set repository config", "err", err, "path", imp.path)
		return nil, err
	}

	client := lfs.NewClient(imp.lfs)
	if client == nil {
		d.logger.Warn("failed to create lfs client: unsupported endpoint", "endpoint", redactRemote(imp.lfsEndpoint))
		return r, nil
	}

	if err := d.fetchImportLFSObjects(ctx, r, client); err != nil {
		d.logger.Error("failed to store missing lfs objects", "err", err, "path", imp.path)
		return nil, err
	}

	return r, nil
}

// fetchImportLFSObjects fetches the LFS objects of an imported repository.
// Like git lfs fetch, it only warns about objects the remote can't provide,
// so one missing object doesn't throw the whole import away. For a mirror,
// the mirror job tries them again on every sync; a plain import never
// fetches them again, so they stay missing until someone pushes them. Any
// other failure fails the import.
func (d *Backend) fetchImportLFSObjects(ctx context.Context, r proto.Repository, client lfs.Client) error {
	err := StoreRepoMissingLFSObjects(ctx, r, d.db, d.store, client)
	var unavailable *lfsObjectsUnavailableError
	if errors.As(err, &unavailable) {
		if r.IsMirror() {
			d.logger.Warn("imported mirror without some lfs objects, mirror syncs will retry them",
				"repo", r.Name(), "missing", unavailable.count)
		} else {
			d.logger.Warn("imported repository without some lfs objects, they will not be fetched again",
				"repo", r.Name(), "missing", unavailable.count)
		}
		return nil
	}
	return err
}

// DeleteRepository deletes a repository.
//
// It implements backend.Backend.
func (d *Backend) DeleteRepository(ctx context.Context, name string) error {
	return d.deleteRepository(ctx, name, nil)
}

// errRepoKept is returned by deleteRepository when its keep check kept the
// repository.
var errRepoKept = errors.New("repository kept")

// deleteRepository is DeleteRepository with an optional check. A non-nil
// keep runs in the transaction that deletes the repository's row, with that
// row, read and locked (see GetRepoByNameForUpdate) before the check; if it
// returns true the repository is kept and errRepoKept is returned. The lock
// holds off collaborator, webhook and LFS rows for the repository and
// changes to the row itself until the transaction ends, so none of those
// can land between the check and the delete. Anything not in the database,
// such as refs, is the caller's to order. With
// keep set, a directory with no row is left alone rather than removed as an
// orphan: the row the caller meant is gone, and the directory may belong to
// a repository being created.
func (d *Backend) deleteRepository(ctx context.Context, name string, keep func(tx *db.Tx, m models.Repo) (bool, error)) error {
	name = utils.SanitizeRepo(name)
	rp := d.repoPath(name)

	// Build the webhook event before deleting. A repository whose directory is
	// gone has no event, but its database row still has to go.
	var wh *webhook.RepositoryEvent
	r, err := d.Repository(ctx, name)
	switch {
	case err == nil:
		ev, err := webhook.NewRepositoryEvent(ctx, proto.UserFromContext(ctx), r, webhook.RepositoryEventActionDelete)
		if err != nil {
			return err
		}
		wh = &ev
	case !errors.Is(err, proto.ErrRepoNotFound):
		return err
	}

	// The subscribers must be read before the rows are removed: the webhooks
	// are gone after the delete. A failure to read them is logged and does not
	// stop the delete.
	var hooks []models.Webhook
	if wh != nil {
		subs, subErr := webhook.Subscribers(ctx, *wh)
		if subErr != nil {
			d.logEventNotDelivered(*wh, subErr)
		} else {
			hooks = subs
		}
	}

	// repoID is set once the repository's row is found, and names its LFS
	// directory.
	var repoID int64
	// rowDeleted is set once the repository's row is deleted, and says the
	// directory is to be removed after the commit.
	var rowDeleted bool
	if err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		// With a keep check, the row is locked before the check, so
		// nothing can be added for the repository between the check and
		// the delete, only to go with the row by ON DELETE CASCADE.
		getRepo := d.store.GetRepoByName
		if keep != nil {
			getRepo = d.store.GetRepoByNameForUpdate
		}
		repom, dberr := getRepo(ctx, tx, name)
		if dberr != nil {
			// A directory with no row is an orphan; remove it.
			if keep == nil && d.repoPathExists(name) {
				return os.RemoveAll(rp)
			}
			if errors.Is(db.WrapError(dberr), db.ErrRecordNotFound) {
				return proto.ErrRepoNotFound
			}
			return db.WrapError(dberr)
		}
		if keep != nil {
			kept, err := keep(tx, repom)
			if err != nil {
				return err
			}
			if kept {
				return errRepoKept
			}
		}

		repoID = repom.ID

		// Remove the webhooks and their rows explicitly, so none are left
		// behind whether or not the connection enforces foreign keys.
		if err := d.deleteRepoWebhookRows(ctx, tx, repom.ID); err != nil {
			return db.WrapError(err)
		}

		if err := d.store.DeleteRepoByName(ctx, tx, name); err != nil {
			return db.WrapError(err)
		}

		rowDeleted = true
		return nil
	}); err != nil {
		return wrapRepoError(err)
	}

	// Remove the directory only now that the row is gone. Removing it inside
	// the transaction could leave a repository with a row but a partly
	// removed directory when the removal failed, or a row with no directory
	// when the commit failed. The delete has happened once the row is gone,
	// so a failure here is logged rather than returned. The leftover
	// directory has no row: it cannot be created, imported or renamed over,
	// and deleting the repository again removes it.
	if rowDeleted {
		if err := os.RemoveAll(rp); err != nil {
			d.logger.Error("failed to delete repository directory", "repo", name, "path", rp, "err", err)
		}
	}

	// Remove the repository's LFS storage only now that its row is gone, so
	// a failed delete leaves its objects in place. This takes the temporary
	// files of unfinished uploads and objects without a row along with the
	// recorded ones. Repository ids are never reused, so no other repository
	// can have written here. The row is already gone, so a failure is logged
	// rather than returned. An LFS upload, import or mirror sync still running
	// for this repository can recreate the directory after it is removed,
	// leaving an orphan; since the id is not reused, no other repository can
	// reach it.
	if repoID != 0 {
		lfsPath := filepath.Join(d.cfg.DataPath, "lfs", strconv.FormatInt(repoID, 10))
		if err := os.RemoveAll(lfsPath); err != nil {
			d.logger.Error("failed to delete lfs objects", "repo", name, "path", lfsPath, "err", err)
		}
	}

	// The repository is gone, so its event is delivered without recording
	// deliveries: they would refer to webhooks that no longer exist. The
	// receiver's outcome is only logged, not returned, so it cannot undo the delete.
	if wh != nil {
		d.deliverAfterChange(ctx, hooks, *wh, false)
	}

	return nil
}

// RenameRepository renames a repository.
//
// It implements backend.Backend.
func (d *Backend) RenameRepository(ctx context.Context, oldName string, newName string) error {
	oldName = utils.SanitizeRepo(oldName)
	if err := utils.ValidateRepo(oldName); err != nil {
		return err
	}

	newName = utils.SanitizeRepo(newName)
	if err := utils.ValidateRepo(newName); err != nil {
		return err
	}

	if oldName == newName {
		return nil
	}

	// Renaming onto a new name creates it, so it needs the same permission.
	// The caller is the user and public key on the context.
	if err := d.checkCanCreate(ctx, proto.UserFromContext(ctx)); err != nil {
		return err
	}

	op := d.repoPath(oldName)
	np := d.repoPath(newName)
	if !d.repoPathExists(oldName) {
		return proto.ErrRepoNotFound
	}

	if d.repoPathExists(newName) {
		return proto.ErrRepoExist
	}

	if err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		if err := d.store.SetRepoNameByName(ctx, tx, oldName, newName); err != nil {
			return err
		}

		// Make sure the new repository parent directory exists.
		if err := os.MkdirAll(filepath.Dir(np), os.ModePerm); err != nil {
			d.logger.Error("failed to create repository parent directory", "repo", newName, "err", err)
			return errRepoUpdate
		}

		if err := os.Rename(op, np); err != nil {
			d.logger.Error("failed to move repository", "repo", oldName, "err", err)
			return errRepoUpdate
		}

		return nil
	}); err != nil {
		return wrapRepoError(err)
	}

	// The rename is committed, so an event problem is logged, not returned.
	user := proto.UserFromContext(ctx)
	repo, err := d.Repository(ctx, newName)
	if err != nil {
		d.logger.Error("rename committed but webhook event skipped", "repo", newName, "err", err)
		return nil
	}

	wh, err := webhook.NewRepositoryEvent(ctx, user, repo, webhook.RepositoryEventActionRename)
	if err != nil {
		d.logger.Error("rename committed but webhook event skipped", "repo", newName, "err", err)
		return nil
	}

	d.sendAfterChange(ctx, wh)
	return nil
}

// Repositories returns a list of repositories per page.
//
// It implements backend.Backend.
func (d *Backend) Repositories(ctx context.Context) ([]proto.Repository, error) {
	repos := make([]proto.Repository, 0)

	if err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		ms, err := d.store.GetAllRepos(ctx, tx)
		if err != nil {
			return err
		}

		for _, m := range ms {
			r := &repo{
				name:   m.Name,
				path:   filepath.Join(d.repoPath(m.Name)),
				repo:   m,
				logger: d.logger,
			}

			repos = append(repos, r)
		}

		return nil
	}); err != nil {
		return nil, db.WrapError(err)
	}

	return repos, nil
}

// Repository returns a repository by name.
//
// It implements backend.Backend.
func (d *Backend) Repository(ctx context.Context, name string) (proto.Repository, error) {
	var m models.Repo
	name = utils.SanitizeRepo(name)

	rp := d.repoPath(name)
	if _, err := os.Stat(rp); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			d.logger.Errorf("failed to stat repository path: %v", err)
		}
		return nil, proto.ErrRepoNotFound
	}

	if err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		var err error
		m, err = d.store.GetRepoByName(ctx, tx, name)
		return err
	}); err != nil {
		return nil, wrapRepoError(err)
	}

	r := &repo{
		name:   name,
		path:   rp,
		repo:   m,
		logger: d.logger,
	}

	return r, nil
}

// errRepoUpdate replaces filesystem errors, which carry the server data
// path, when a repository setting cannot be written.
var errRepoUpdate = errors.New("failed to update repository")

// wrapRepoError maps a missing database row to proto.ErrRepoNotFound so
// callers never see the raw driver error for a repository that does not
// exist.
func wrapRepoError(err error) error {
	err = db.WrapError(err)
	if errors.Is(err, db.ErrRecordNotFound) {
		return proto.ErrRepoNotFound
	}
	return err
}

// Description returns the description of a repository.
//
// It implements backend.Backend.
func (d *Backend) Description(ctx context.Context, name string) (string, error) {
	name = utils.SanitizeRepo(name)
	var desc string
	if err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		var err error
		desc, err = d.store.GetRepoDescriptionByName(ctx, tx, name)
		return err
	}); err != nil {
		return "", wrapRepoError(err)
	}

	return desc, nil
}

// IsMirror returns true if the repository is a mirror.
//
// It implements backend.Backend.
func (d *Backend) IsMirror(ctx context.Context, name string) (bool, error) {
	name = utils.SanitizeRepo(name)
	var mirror bool
	if err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		var err error
		mirror, err = d.store.GetRepoIsMirrorByName(ctx, tx, name)
		return err
	}); err != nil {
		return false, wrapRepoError(err)
	}
	return mirror, nil
}

// IsPrivate returns true if the repository is private.
//
// It implements backend.Backend.
func (d *Backend) IsPrivate(ctx context.Context, name string) (bool, error) {
	name = utils.SanitizeRepo(name)
	var private bool
	if err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		var err error
		private, err = d.store.GetRepoIsPrivateByName(ctx, tx, name)
		return err
	}); err != nil {
		return false, wrapRepoError(err)
	}

	return private, nil
}

// IsHidden returns true if the repository is hidden.
//
// It implements backend.Backend.
func (d *Backend) IsHidden(ctx context.Context, name string) (bool, error) {
	name = utils.SanitizeRepo(name)
	var hidden bool
	if err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		var err error
		hidden, err = d.store.GetRepoIsHiddenByName(ctx, tx, name)
		return err
	}); err != nil {
		return false, wrapRepoError(err)
	}

	return hidden, nil
}

// ProjectName returns the project name of a repository.
//
// It implements backend.Backend.
func (d *Backend) ProjectName(ctx context.Context, name string) (string, error) {
	name = utils.SanitizeRepo(name)
	var pname string
	if err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		var err error
		pname, err = d.store.GetRepoProjectNameByName(ctx, tx, name)
		return err
	}); err != nil {
		return "", wrapRepoError(err)
	}

	return pname, nil
}

// SetHidden sets the hidden flag of a repository.
//
// It implements backend.Backend.
func (d *Backend) SetHidden(ctx context.Context, name string, hidden bool) error {
	name = utils.SanitizeRepo(name)

	// An UPDATE that matches no row succeeds silently, so look the
	// repository up first.
	return wrapRepoError(d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		if _, err := d.store.GetRepoByName(ctx, tx, name); err != nil {
			return err
		}
		return d.store.SetRepoIsHiddenByName(ctx, tx, name, hidden)
	}))
}

// mirrorCheckTimeout bounds the hostname lookups SetMirror makes when it
// validates a repository's remotes before switching mirroring on.
const mirrorCheckTimeout = 10 * time.Second

// errRepoChangedDuringMirror is returned when the repository was replaced,
// re-owned or re-pointed while SetMirror was validating its remotes.
var errRepoChangedDuringMirror = errors.New("repository changed while switching mirroring; try again")

// SetMirror sets the mirror flag of a repository.
// Note: enabling mirror mode requires the repository to have been imported
// with a remote URL. Use ImportRepository to create a new mirror.
func (d *Backend) SetMirror(ctx context.Context, name string, mirror bool) error {
	name = utils.SanitizeRepo(name)
	rp := d.repoPath(name)

	// The remotes are validated before the transaction opens. Validation
	// may resolve hostnames, and on SQLite a read inside a transaction holds
	// the database lock until it ends, so a slow resolver would otherwise
	// stall every writer on the server.
	m, err := d.store.GetRepoByName(ctx, d.db, name)
	if err != nil {
		return wrapRepoError(err)
	}
	_, rcfg, err := d.openRepoConfig(rp)
	if err != nil {
		return err
	}
	urls := remoteURLs(rcfg)
	lfsURL := rcfg.Section("lfs").Option("url")
	if mirror && len(urls) == 0 {
		return errors.New("cannot enable mirror mode: repository has no remote URL configured")
	}
	if mirror {
		cctx, cancel := context.WithTimeout(ctx, mirrorCheckTimeout)
		err := d.checkMirrorRemotes(cctx, m, rcfg)
		cancel()
		if err != nil {
			return wrapRepoError(err)
		}
	}

	return wrapRepoError(d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		// Re-read both, so what is written is what was validated.
		cur, err := d.store.GetRepoByName(ctx, tx, name)
		if err != nil {
			return err
		}
		r, rcfg, err := d.openRepoConfig(rp)
		if err != nil {
			return err
		}
		if cur.ID != m.ID || cur.UserID != m.UserID ||
			!slices.Equal(remoteURLs(rcfg), urls) || rcfg.Section("lfs").Option("url") != lfsURL {
			return errRepoChangedDuringMirror
		}

		setRemotesMirror(rcfg, mirror)

		// The flag is written first and the git config last, so a failure
		// writing the config rolls the flag back and the two stay in step.
		if err := d.store.SetRepoIsMirrorByName(ctx, tx, name, mirror); err != nil {
			return err
		}
		if err := r.SetConfig(rcfg); err != nil {
			d.logger.Error("failed to set repository config", "err", err, "path", rp)
			return errRepoUpdate
		}
		return nil
	}))
}

// openRepoConfig opens the repository at rp and reads its git config. The
// raw errors carry the server data path, so they are logged and a generic
// one is returned.
func (d *Backend) openRepoConfig(rp string) (*git.Repository, *gitcfg.Config, error) {
	r, err := git.Open(rp)
	if err != nil {
		d.logger.Error("failed to open repository", "err", err, "path", rp)
		return nil, nil, errRepoUpdate
	}
	rcfg, err := r.Config()
	if err != nil {
		d.logger.Error("failed to read repository config", "err", err, "path", rp)
		return nil, nil, errRepoUpdate
	}
	return r, rcfg, nil
}

// remoteURLs returns the URL of every remote in rcfg that has one, in
// config order.
func remoteURLs(rcfg *gitcfg.Config) []string {
	var urls []string
	for _, sub := range rcfg.Section("remote").Subsections {
		if u := sub.Option("url"); u != "" {
			urls = append(urls, u)
		}
	}
	return urls
}

// setRemotesMirror sets, or with mirror false removes, the mirror option on
// every remote in rcfg that has a URL.
func setRemotesMirror(rcfg *gitcfg.Config, mirror bool) {
	for _, sub := range rcfg.Section("remote").Subsections {
		if sub.Option("url") == "" {
			continue
		}
		if mirror {
			sub.SetOption("mirror", "true")
		} else {
			sub.RemoveOption("mirror")
		}
	}
}

// checkMirrorRemotes refuses to switch on mirroring for repo m, whose git
// config is rcfg, when the mirror job would skip it on every run. The job
// validates every remote with RemotePolicy for the repository's owner (see
// pkg/jobs), so the same check is made here. It matters for a repository
// imported without --mirror by a caller whose admin key let it reach an ssh
// host its non-admin owner account may not.
//
// Like the job, the stored LFS endpoint is checked only when LFS is enabled,
// and a failure there is not a refusal: the job still syncs the git data and
// skips only the LFS objects, so it is logged as a warning.
//
// ctx bounds the hostname lookups validation makes. No transaction may be
// open across this call, for the same reason.
func (d *Backend) checkMirrorRemotes(ctx context.Context, m models.Repo, rcfg *gitcfg.Config) error {
	// As in the job, an owner that cannot be looked up is nobody.
	var owner proto.User
	if m.UserID.Valid {
		um, err := d.store.GetUserByID(ctx, d.db, m.UserID.Int64)
		if err == nil {
			owner = &user{user: um}
		} else if !errors.Is(db.WrapError(err), db.ErrRecordNotFound) {
			return err
		}
	}
	policy := RemotePolicy(d.cfg, owner)

	for _, u := range remoteURLs(rcfg) {
		_, err := validateImportRemote(ctx, u, policy)
		if errors.Is(err, proto.ErrUnauthorized) {
			d.logger.Warn("refusing to enable mirror: owner is not an admin, so the mirror job could not sync it",
				"repo", m.Name, "remote", redactRemote(u))
			return errMirrorOwnerNotAdmin
		}
		if err != nil {
			return err
		}
	}

	if u := rcfg.Section("lfs").Option("url"); d.cfg.LFS.Enabled && u != "" {
		if _, err := validateImportRemote(ctx, u, policy); err != nil {
			d.logger.Warn("enabling mirror, but the mirror job will skip LFS objects: endpoint failed validation",
				"repo", m.Name, "endpoint", redactRemote(u), "err", err)
		}
	}
	return nil
}

// SetDescription sets the description of a repository.
//
// It implements backend.Backend.
func (d *Backend) SetDescription(ctx context.Context, name string, desc string) error {
	name = utils.SanitizeRepo(name)
	desc = utils.Sanitize(desc)
	rp := d.repoPath(name)

	return wrapRepoError(d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		if _, err := d.store.GetRepoByName(ctx, tx, name); err != nil {
			return err
		}

		// The raw error carries the server data path, so log it and return
		// a generic one.
		if err := os.WriteFile(filepath.Join(rp, "description"), []byte(desc), fs.ModePerm); err != nil {
			d.logger.Error("failed to write description", "repo", name, "err", err)
			return errRepoUpdate
		}

		return d.store.SetRepoDescriptionByName(ctx, tx, name, desc)
	}))
}

// SetPrivate sets the private flag of a repository.
//
// It implements backend.Backend.
func (d *Backend) SetPrivate(ctx context.Context, name string, private bool) error {
	name = utils.SanitizeRepo(name)
	rp := d.repoPath(name)

	var changed bool
	if err := wrapRepoError(
		d.db.TransactionContext(ctx, func(tx *db.Tx) error {
			m, err := d.store.GetRepoByName(ctx, tx, name)
			if err != nil {
				return err
			}
			changed = m.Private != private

			fp := filepath.Join(rp, "git-daemon-export-ok")
			if !private {
				if err := os.WriteFile(fp, []byte{}, fs.ModePerm); err != nil {
					d.logger.Error("failed to write git-daemon-export-ok", "repo", name, "err", err)
					return errRepoUpdate
				}
			} else {
				if _, err := os.Stat(fp); err == nil {
					if err := os.Remove(fp); err != nil {
						d.logger.Error("failed to remove git-daemon-export-ok", "repo", name, "err", err)
						return errRepoUpdate
					}
				}
			}

			return d.store.SetRepoIsPrivateByName(ctx, tx, name, private)
		}),
	); err != nil {
		return err
	}

	if !changed {
		return nil
	}

	// The change is committed, so an event problem is logged, not returned.
	repo, err := d.Repository(ctx, name)
	if err != nil {
		d.logger.Error("visibility change committed but webhook event skipped", "repo", name, "err", err)
		return nil
	}

	wh, err := webhook.NewRepositoryEvent(ctx, proto.UserFromContext(ctx), repo, webhook.RepositoryEventActionVisibilityChange)
	if err != nil {
		d.logger.Error("visibility change committed but webhook event skipped", "repo", name, "err", err)
		return nil
	}

	d.sendAfterChange(ctx, wh)
	return nil
}

// SetProjectName sets the project name of a repository.
//
// It implements backend.Backend.
func (d *Backend) SetProjectName(ctx context.Context, repo string, name string) error {
	repo = utils.SanitizeRepo(repo)
	name = utils.Sanitize(name)

	return wrapRepoError(
		d.db.TransactionContext(ctx, func(tx *db.Tx) error {
			if _, err := d.store.GetRepoByName(ctx, tx, repo); err != nil {
				return err
			}
			return d.store.SetRepoProjectNameByName(ctx, tx, repo, name)
		}),
	)
}

// repoPath returns the path to a repository.
func (d *Backend) repoPath(name string) string {
	name = utils.SanitizeRepo(name)
	rn := strings.ReplaceAll(name, "/", string(os.PathSeparator))
	return filepath.Join(filepath.Join(d.cfg.DataPath, "repos"), rn+".git")
}

// repoPathExists reports whether anything occupies the repository's directory,
// whether or not a database row points at it.
func (d *Backend) repoPathExists(name string) bool {
	_, err := os.Lstat(d.repoPath(name))
	return err == nil
}

var _ proto.Repository = (*repo)(nil)

// repo is a Git repository with metadata stored in a SQLite database.
type repo struct {
	name   string
	path   string
	repo   models.Repo
	logger *log.Logger
}

// ID returns the repository's ID.
//
// It implements proto.Repository.
func (r *repo) ID() int64 {
	return r.repo.ID
}

// UserID returns the repository's owner's user ID.
// If the repository is not owned by anyone, it returns 0.
//
// It implements proto.Repository.
func (r *repo) UserID() int64 {
	if r.repo.UserID.Valid {
		return r.repo.UserID.Int64
	}
	return 0
}

// Description returns the repository's description.
//
// It implements backend.Repository.
func (r *repo) Description() string {
	return r.repo.Description
}

// IsMirror returns whether the repository is a mirror.
//
// It implements backend.Repository.
func (r *repo) IsMirror() bool {
	return r.repo.Mirror
}

// IsPrivate returns whether the repository is private.
//
// It implements backend.Repository.
func (r *repo) IsPrivate() bool {
	return r.repo.Private
}

// Name returns the repository's name.
//
// It implements backend.Repository.
func (r *repo) Name() string {
	return r.name
}

// Open opens the repository.
//
// It implements backend.Repository.
func (r *repo) Open() (*git.Repository, error) {
	gr, err := git.Open(r.path)
	if err != nil {
		return nil, openRepoError(r.logger, err, r.path)
	}
	return gr, nil
}

// openRepoError returns the error to show to clients when git does not accept
// a repository. git's reason (for example "detected dubious ownership") names
// the server path, so it is logged here and the client gets the plain
// ErrNotAGitRepository, as before. Other errors are returned unchanged.
func openRepoError(logger *log.Logger, err error, path string) error {
	if !errors.Is(err, git.ErrNotAGitRepository) {
		return err
	}
	if logger == nil {
		logger = log.Default()
	}
	logger.Error("git does not accept the repository", "err", err, "path", path)
	return git.ErrNotAGitRepository
}

// ProjectName returns the repository's project name.
//
// It implements backend.Repository.
func (r *repo) ProjectName() string {
	return r.repo.ProjectName
}

// IsHidden returns whether the repository is hidden.
//
// It implements backend.Repository.
func (r *repo) IsHidden() bool {
	return r.repo.Hidden
}

// CreatedAt returns the repository's creation time.
func (r *repo) CreatedAt() time.Time {
	return r.repo.CreatedAt
}

// UpdatedAt returns the repository's last update time.
func (r *repo) UpdatedAt() time.Time {
	// Try to read the last modified time from the info directory.
	if t, err := readOneline(filepath.Join(r.path, "info", "last-modified")); err == nil {
		if t, err := time.Parse(time.RFC3339, t); err == nil {
			return t
		}
	}

	rr, err := git.Open(r.path)
	if err == nil {
		t, err := rr.LatestCommitTime()
		if err == nil {
			return t
		}
	}

	return r.repo.UpdatedAt
}

func (r *repo) writeLastModified(t time.Time) error {
	fp := filepath.Join(r.path, "info", "last-modified")
	if err := os.MkdirAll(filepath.Dir(fp), os.ModePerm); err != nil {
		return err
	}

	return os.WriteFile(fp, []byte(t.Format(time.RFC3339)), os.ModePerm) //nolint:gosec
}

func readOneline(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}

	defer f.Close() //nolint: errcheck
	s := bufio.NewScanner(f)
	s.Scan()
	return s.Text(), s.Err()
}
