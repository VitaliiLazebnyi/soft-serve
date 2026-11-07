package git

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strconv"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/git-lfs-transfer/transfer"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/storage"
	"github.com/charmbracelet/soft-serve/pkg/store"
)

// lfsTransfer implements transfer.Backend.
type lfsTransfer struct {
	ctx     context.Context
	cfg     *config.Config
	dbx     *db.DB
	store   store.Store
	logger  *log.Logger
	storage storage.Storage
	repo    proto.Repository
	// op is the session operation (download or upload). The SSH command
	// authorizes only the operation, and the transfer processor accepts
	// put-object and lock in either, so write commands check it here.
	op string
}

var _ transfer.Backend = &lfsTransfer{}

// errInvalidOid is returned when a client sends a malformed object ID. Object
// IDs are interpolated into storage paths, so anything outside the SHA-256
// alphabet is a path traversal attempt.
//
// It wraps transfer.ErrParseError so the processor answers with a 400 instead
// of falling through to a generic internal error, and lfs.ErrInvalidOIDFormat
// so callers can still test for the cause.
var errInvalidOid = fmt.Errorf("%w: %w", transfer.ErrParseError, lfs.ErrInvalidOIDFormat)

// errReadOnlySession is returned for a write command (put-object, lock,
// unlock) sent in a download session, which needs only read access.
var errReadOnlySession = fmt.Errorf("%w: not allowed in a download session", transfer.ErrForbidden)

// LFSTransfer is a Git LFS transfer service handler.
// ctx is expected to have proto.User, *backend.Backend, *log.Logger,
// *config.Config, *db.DB, and store.Store.
// The first arg in cmd.Args should be the repo path.
// The second arg in cmd.Args should be the LFS operation (download or upload).
func LFSTransfer(ctx context.Context, cmd ServiceCommand) error {
	if len(cmd.Args) < 2 {
		return errors.New("missing args")
	}

	op := cmd.Args[1]
	if op != lfs.OperationDownload && op != lfs.OperationUpload {
		return errors.New("invalid operation")
	}

	logger := log.FromContext(ctx).WithPrefix("lfs-transfer")
	handler := transfer.NewPktline(cmd.Stdin, cmd.Stdout, &lfsLogger{logger})
	repo := proto.RepositoryFromContext(ctx)
	if repo == nil {
		logger.Error("no repository in context")
		return proto.ErrRepoNotFound
	}

	// Advertise capabilities.
	for _, cap := range transfer.Capabilities {
		if err := handler.WritePacketText(cap); err != nil {
			logger.Errorf("error sending capability: %s: %v", cap, err)
			return err
		}
	}

	if err := handler.WriteFlush(); err != nil {
		logger.Error("error sending flush", "err", err)
		return err
	}

	repoID := strconv.FormatInt(repo.ID(), 10)
	cfg := config.FromContext(ctx)
	processor := transfer.NewProcessor(handler, &lfsTransfer{
		ctx:     ctx,
		cfg:     cfg,
		dbx:     db.FromContext(ctx),
		store:   store.FromContext(ctx),
		logger:  logger,
		storage: storage.NewLocalStorage(filepath.Join(cfg.DataPath, "lfs", repoID)),
		repo:    repo,
		op:      op,
	}, &lfsLogger{logger})

	return processor.ProcessCommands(op)
}

// Batch implements transfer.Backend.
func (t *lfsTransfer) Batch(_ string, pointers []transfer.BatchItem, _ transfer.Args) ([]transfer.BatchItem, error) {
	for i := range pointers {
		p := transfer.Pointer{Oid: pointers[i].Oid, Size: pointers[i].Size}
		if !p.IsValid() {
			return pointers, errInvalidOid
		}
	}

	for i := range pointers {
		obj, err := t.store.GetLFSObjectByOid(t.ctx, t.dbx, t.repo.ID(), pointers[i].Oid)
		if err != nil && !errors.Is(err, db.ErrRecordNotFound) {
			return pointers, db.WrapError(err)
		}

		pointers[i].Present, err = t.storage.Exists(path.Join("objects", pointers[i].RelativePath()))
		if err != nil {
			return pointers, err
		}

		if pointers[i].Present && obj.ID == 0 {
			if err := t.store.CreateLFSObject(t.ctx, t.dbx, t.repo.ID(), pointers[i].Oid, pointers[i].Size); err != nil {
				return pointers, db.WrapError(err)
			}
		}
	}

	return pointers, nil
}

// Download implements transfer.Backend.
func (t *lfsTransfer) Download(oid string, _ transfer.Args) (io.ReadCloser, int64, error) {
	p := transfer.Pointer{Oid: oid}
	if !p.IsValid() {
		return nil, 0, errInvalidOid
	}

	cfg := config.FromContext(t.ctx)
	repoID := strconv.FormatInt(t.repo.ID(), 10)
	strg := storage.NewLocalStorage(filepath.Join(cfg.DataPath, "lfs", repoID))
	pointer := transfer.Pointer{Oid: oid}
	obj, err := strg.Open(path.Join("objects", pointer.RelativePath()))
	if err != nil {
		return nil, 0, err
	}
	stat, err := obj.Stat()
	if err != nil {
		return nil, 0, err
	}
	return obj, stat.Size(), nil
}

// Upload implements transfer.Backend.
func (t *lfsTransfer) Upload(oid string, size int64, r io.Reader, _ transfer.Args) error {
	p := transfer.Pointer{Oid: oid}
	if !p.IsValid() {
		return errInvalidOid
	}

	if t.op == lfs.OperationDownload {
		// Consume the object data so the next command is read from the
		// right place in the stream; nothing is stored.
		if r != nil {
			_, _ = io.Copy(io.Discard, r)
		}
		return errReadOnlySession
	}

	if r == nil {
		return fmt.Errorf("no reader: %w", transfer.ErrMissingData)
	}

	if size < 0 {
		_, _ = io.Copy(io.Discard, r)
		return fmt.Errorf("%w: invalid object size %d", transfer.ErrParseError, size)
	}

	tempDir := "incomplete"
	randBytes := make([]byte, 12)
	if _, err := rand.Read(randBytes); err != nil {
		_, _ = io.Copy(io.Discard, r)
		return err
	}

	tempName := fmt.Sprintf("%s%x", oid, randBytes)
	tempName = path.Join(tempDir, tempName)
	defer t.storage.Delete(tempName) //nolint: errcheck

	// Hash the data while writing it to a temporary file, and read at most
	// one byte past the declared size so an oversized object is not written
	// out in full. Whatever is left is drained so the next command is read
	// from the right place in the stream.
	hash := sha256.New()
	written, err := t.storage.Put(tempName, io.TeeReader(io.LimitReader(r, size+1), hash))
	_, _ = io.Copy(io.Discard, r)
	if err != nil {
		t.logger.Errorf("error putting object: %v", err)
		return err
	}
	if written != size {
		return fmt.Errorf("%w: invalid object size, expected %d, got at least %d", transfer.ErrCorruptData, size, written)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != oid {
		return fmt.Errorf("%w: invalid object ID, expected %s, got %s", transfer.ErrCorruptData, oid, got)
	}

	// The content is verified, so move it into place before recording it. A
	// concurrent upload of the same object renames identical bytes over the
	// same path. Rename takes names relative to the storage root, not the
	// absolute path the temp file happens to live at.
	pointer := transfer.Pointer{Oid: oid, Size: size}
	expectedPath := path.Join("objects", pointer.RelativePath())
	if err := t.storage.Rename(tempName, expectedPath); err != nil {
		// On Windows, renaming onto a file that a concurrent upload of the
		// same object is renaming or has open fails. The data was checked
		// against the oid that names expectedPath, so a file of the expected
		// size there is the same object.
		fi, serr := t.storage.Stat(expectedPath)
		if serr != nil || !fi.Mode().IsRegular() || fi.Size() != size {
			t.logger.Errorf("error renaming object: %v", err)
			return err
		}
	}

	if err := t.store.CreateLFSObject(t.ctx, t.dbx, t.repo.ID(), pointer.Oid, pointer.Size); err != nil {
		// The object may already be recorded, by an earlier or concurrent
		// upload. WrapError also maps foreign key violations to
		// ErrDuplicateKey, so only treat it as success if the row is there.
		if !errors.Is(err, db.ErrDuplicateKey) {
			return err
		}
		if _, gerr := t.store.GetLFSObjectByOid(t.ctx, t.dbx, t.repo.ID(), pointer.Oid); gerr != nil {
			t.logger.Errorf("error recording object: %v (lookup: %v)", err, gerr)
			return err
		}
	}

	return nil
}

// Verify implements transfer.Backend.
func (t *lfsTransfer) Verify(oid string, size int64, _ transfer.Args) (transfer.Status, error) {
	p := transfer.Pointer{Oid: oid}
	if !p.IsValid() {
		return transfer.NewStatus(transfer.StatusConflict, "invalid OID format"), nil
	}

	obj, err := t.store.GetLFSObjectByOid(t.ctx, t.dbx, t.repo.ID(), oid)
	if err != nil {
		if errors.Is(err, db.ErrRecordNotFound) {
			return transfer.NewStatus(transfer.StatusNotFound, "object not found"), nil
		}
		t.logger.Errorf("error getting object: %v", err)
		return nil, err
	}

	if obj.Size != size {
		t.logger.Errorf("size mismatch: %d != %d", obj.Size, size)
		return transfer.NewStatus(transfer.StatusConflict, "size mismatch"), nil
	}

	return transfer.SuccessStatus(), nil
}

type lfsLockBackend struct {
	*lfsTransfer
	args map[string]string
	user proto.User
}

var _ transfer.LockBackend = (*lfsLockBackend)(nil)

// LockBackend implements transfer.Backend.
func (t *lfsTransfer) LockBackend(args transfer.Args) transfer.LockBackend {
	user := proto.UserFromContext(t.ctx)
	if user == nil {
		// Locks are owned by an account. A caller without one (an anonymous
		// key, or an initial_admin_keys key with no user row) can't hold or
		// list them. Returning nil here would make the transfer processor
		// panic on the first lock command, so refuse each one instead.
		t.logger.Debugf("no user in context, refusing lock commands, repo %s", t.repo.Name())
		return noUserLockBackend{}
	}

	return &lfsLockBackend{t, args, user}
}

// noUserLockBackend is the lock backend for a caller with no account. Every
// method returns transfer.ErrForbidden, which the processor sends back to the
// client as a 403 status.
type noUserLockBackend struct{}

var _ transfer.LockBackend = noUserLockBackend{}

// Create implements transfer.LockBackend.
func (noUserLockBackend) Create(string, string) (transfer.Lock, error) {
	return nil, errNoUserLocks
}

// Unlock implements transfer.LockBackend.
func (noUserLockBackend) Unlock(transfer.Lock) error {
	return errNoUserLocks
}

// FromPath implements transfer.LockBackend.
func (noUserLockBackend) FromPath(string) (transfer.Lock, error) {
	return nil, errNoUserLocks
}

// FromID implements transfer.LockBackend.
func (noUserLockBackend) FromID(string) (transfer.Lock, error) {
	return nil, errNoUserLocks
}

// Range implements transfer.LockBackend.
func (noUserLockBackend) Range(string, int, func(transfer.Lock) error) (string, error) {
	return "", errNoUserLocks
}

var errNoUserLocks = fmt.Errorf("%w: locks need a user account", transfer.ErrForbidden)

// Create implements transfer.LockBackend.
func (l *lfsLockBackend) Create(path string, refname string) (transfer.Lock, error) {
	if l.op == lfs.OperationDownload {
		return nil, errReadOnlySession
	}

	var lock LFSLock
	if err := l.dbx.TransactionContext(l.ctx, func(tx *db.Tx) error {
		if err := l.store.CreateLFSLockForUser(l.ctx, tx, l.repo.ID(), l.user.ID(), path, refname); err != nil {
			return db.WrapError(err)
		}

		var err error
		lock.lock, err = l.store.GetLFSLockForUserPath(l.ctx, tx, l.repo.ID(), l.user.ID(), path)
		if err != nil {
			return db.WrapError(err)
		}

		lock.owner, err = l.store.GetUserByID(l.ctx, tx, lock.lock.UserID)
		return db.WrapError(err)
	}); err != nil {
		// Return conflict (409) if the lock already exists.
		if errors.Is(err, db.ErrDuplicateKey) {
			return nil, transfer.ErrConflict
		}
		l.logger.Errorf("error creating lock: %v", err)
		return nil, err
	}

	lock.backend = l

	return &lock, nil
}

// FromID implements transfer.LockBackend.
func (l *lfsLockBackend) FromID(id string) (transfer.Lock, error) {
	var lock LFSLock
	iid, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, err
	}

	if err := l.dbx.TransactionContext(l.ctx, func(tx *db.Tx) error {
		var err error
		lock.lock, err = l.store.GetLFSLockForUserByID(l.ctx, tx, l.repo.ID(), l.user.ID(), iid)
		if err != nil {
			return db.WrapError(err)
		}

		lock.owner, err = l.store.GetUserByID(l.ctx, tx, lock.lock.UserID)
		return db.WrapError(err)
	}); err != nil {
		if errors.Is(err, db.ErrRecordNotFound) {
			return nil, transfer.ErrNotFound
		}
		l.logger.Errorf("error getting lock: %v", err)
		return nil, err
	}

	lock.backend = l

	return &lock, nil
}

// FromPath implements transfer.LockBackend.
func (l *lfsLockBackend) FromPath(path string) (transfer.Lock, error) {
	var lock LFSLock

	if err := l.dbx.TransactionContext(l.ctx, func(tx *db.Tx) error {
		var err error
		lock.lock, err = l.store.GetLFSLockForUserPath(l.ctx, tx, l.repo.ID(), l.user.ID(), path)
		if err != nil {
			return db.WrapError(err)
		}

		lock.owner, err = l.store.GetUserByID(l.ctx, tx, lock.lock.UserID)
		return db.WrapError(err)
	}); err != nil {
		if errors.Is(err, db.ErrRecordNotFound) {
			return nil, transfer.ErrNotFound
		}
		l.logger.Errorf("error getting lock: %v", err)
		return nil, err
	}

	lock.backend = l

	return &lock, nil
}

// Range implements transfer.LockBackend.
func (l *lfsLockBackend) Range(cursor string, limit int, fn func(transfer.Lock) error) (string, error) {
	var nextCursor string
	var locks []*LFSLock

	page, _ := strconv.Atoi(cursor)
	if page <= 0 {
		page = 1
	}

	if limit <= 0 {
		limit = lfs.DefaultLocksLimit
	} else if limit > 100 {
		limit = 100
	}

	if err := l.dbx.TransactionContext(l.ctx, func(tx *db.Tx) error {
		l.logger.Debug("getting locks", "limit", limit, "page", page)
		mlocks, err := l.store.GetLFSLocks(l.ctx, tx, l.repo.ID(), page, limit)
		if err != nil {
			return db.WrapError(err)
		}

		if len(mlocks) == limit {
			nextCursor = strconv.Itoa(page + 1)
		}

		users := make(map[int64]models.User, 0)
		for _, mlock := range mlocks {
			owner, ok := users[mlock.UserID]
			if !ok {
				owner, err = l.store.GetUserByID(l.ctx, tx, mlock.UserID)
				if err != nil {
					return db.WrapError(err)
				}

				users[mlock.UserID] = owner
			}

			locks = append(locks, &LFSLock{lock: mlock, owner: owner, backend: l})
		}

		return nil
	}); err != nil {
		return "", err
	}

	for _, lock := range locks {
		if err := fn(lock); err != nil {
			return "", err
		}
	}

	return nextCursor, nil
}

// Unlock implements transfer.LockBackend.
func (l *lfsLockBackend) Unlock(lock transfer.Lock) error {
	if l.op == lfs.OperationDownload {
		return errReadOnlySession
	}

	id, err := strconv.ParseInt(lock.ID(), 10, 64)
	if err != nil {
		return err
	}

	err = l.dbx.TransactionContext(l.ctx, func(tx *db.Tx) error {
		return db.WrapError(
			l.store.DeleteLFSLockForUserByID(l.ctx, tx, l.repo.ID(), l.user.ID(), id),
		)
	})
	if err != nil {
		if errors.Is(err, db.ErrRecordNotFound) {
			return transfer.ErrNotFound
		}
		l.logger.Error("error unlocking lock", "err", err)
		return err
	}

	return nil
}

// LFSLock is a Git LFS lock object.
// It implements transfer.Lock.
type LFSLock struct {
	lock    models.LFSLock
	owner   models.User
	backend *lfsLockBackend
}

var _ transfer.Lock = (*LFSLock)(nil)

// AsArguments implements transfer.Lock.
func (l *LFSLock) AsArguments() []string {
	return []string{
		fmt.Sprintf("id=%s", l.ID()),
		fmt.Sprintf("path=%s", l.Path()),
		fmt.Sprintf("locked-at=%s", l.FormattedTimestamp()),
		fmt.Sprintf("ownername=%s", l.OwnerName()),
	}
}

// AsLockSpec implements transfer.Lock.
func (l *LFSLock) AsLockSpec(ownerID bool) ([]string, error) {
	id := l.ID()
	spec := []string{
		fmt.Sprintf("lock %s", id),
		fmt.Sprintf("path %s %s", id, l.Path()),
		fmt.Sprintf("locked-at %s %s", id, l.FormattedTimestamp()),
		fmt.Sprintf("ownername %s %s", id, l.OwnerName()),
	}

	if ownerID {
		who := "theirs"
		if l.lock.UserID == l.owner.ID {
			who = "ours"
		}

		spec = append(spec, fmt.Sprintf("owner %s %s", id, who))
	}

	return spec, nil
}

// FormattedTimestamp implements transfer.Lock.
func (l *LFSLock) FormattedTimestamp() string {
	return l.lock.CreatedAt.Format(time.RFC3339)
}

// ID implements transfer.Lock.
func (l *LFSLock) ID() string {
	return strconv.FormatInt(l.lock.ID, 10)
}

// OwnerName implements transfer.Lock.
func (l *LFSLock) OwnerName() string {
	return l.owner.Username
}

// Path implements transfer.Lock.
func (l *LFSLock) Path() string {
	return l.lock.Path
}

// Unlock implements transfer.Lock.
func (l *LFSLock) Unlock() error {
	return l.backend.Unlock(l)
}
