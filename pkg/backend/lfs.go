package backend

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"charm.land/log/v2"
	gitm "github.com/aymanbagabas/git-module"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/storage"
	"github.com/charmbracelet/soft-serve/pkg/store"
)

// ErrLFSObjectsUnavailable is returned when the remote could not provide some
// LFS objects. Every other object was still stored. The error that wraps it
// carries the number of objects; each one is logged on its own.
var ErrLFSObjectsUnavailable = errors.New("some lfs objects are unavailable")

// lfsObjectsUnavailableError is ErrLFSObjectsUnavailable with the number of
// objects that were unavailable.
type lfsObjectsUnavailableError struct {
	count int
}

func (e *lfsObjectsUnavailableError) Error() string {
	return fmt.Sprintf("%s: %d", ErrLFSObjectsUnavailable, e.count)
}

func (e *lfsObjectsUnavailableError) Unwrap() error {
	return ErrLFSObjectsUnavailable
}

// lfsScannedRefsFile is the file in a repository's LFS directory that holds
// the ref tips whose LFS objects were all stored by the last complete sync,
// one object id per line. It lives next to the objects, so it goes away with
// them if the directory is removed.
const lfsScannedRefsFile = "scanned-refs"

// searchPointerBlobs scans for pointers. Tests replace it to see what is
// scanned.
var searchPointerBlobs = lfs.SearchPointerBlobsBetween

// lfsScanError is an error of the pointer scan itself, as opposed to one
// that came up storing the objects it found.
type lfsScanError struct {
	err error
}

func (e *lfsScanError) Error() string { return e.err.Error() }
func (e *lfsScanError) Unwrap() error { return e.err }

// StoreRepoMissingLFSObjects stores missing LFS objects for a repository. It
// scans the whole history of the repository.
//
// Objects the remote can't provide are logged and skipped, and counted in an
// error wrapping ErrLFSObjectsUnavailable once every other object is stored.
// Any other error stops the sync.
func StoreRepoMissingLFSObjects(ctx context.Context, repo proto.Repository, dbx *db.DB, store store.Store, lfsClient lfs.Client) error {
	return storeRepoLFSObjects(ctx, repo, dbx, store, lfsClient, false)
}

// SyncRepoLFSObjects is StoreRepoMissingLFSObjects for mirror syncs. It only
// scans the history added since the last sync that stored every object, so a
// sync that fetched nothing new scans nothing. It scans the whole history
// when no sync completed yet, when the repository's LFS directory was
// removed, or when the shorter scan fails. Objects that were unavailable
// keep the history they are in scanned on later syncs, until a sync stores
// them all.
func SyncRepoLFSObjects(ctx context.Context, repo proto.Repository, dbx *db.DB, store store.Store, lfsClient lfs.Client) error {
	return storeRepoLFSObjects(ctx, repo, dbx, store, lfsClient, true)
}

func storeRepoLFSObjects(ctx context.Context, repo proto.Repository, dbx *db.DB, store store.Store, lfsClient lfs.Client, incremental bool) error {
	cfg := config.FromContext(ctx)
	repoID := strconv.FormatInt(repo.ID(), 10)
	lfsRoot := filepath.Join(cfg.DataPath, "lfs", repoID)
	logger := log.FromContext(ctx).WithPrefix("lfs")

	r, err := repo.Open()
	if err != nil {
		return err
	}

	// The tips are taken before the scan, so refs that move during it are
	// scanned on the next sync.
	tips, err := refTips(ctx, r)
	if err != nil {
		return err
	}

	markerPath := filepath.Join(lfsRoot, lfsScannedRefsFile)
	var since []string
	if incremental {
		since = readScannedRefs(markerPath)
	}

	switch {
	case since == nil:
		err = storeMissingLFSObjects(ctx, repo, r, dbx, store, lfsClient, lfsRoot, tips, nil)
	case slices.Equal(since, tips):
		// Nothing changed since the last complete sync.
		return nil
	default:
		err = storeMissingLFSObjects(ctx, repo, r, dbx, store, lfsClient, lfsRoot, tips, since)
		var scanErr *lfsScanError
		if errors.As(err, &scanErr) && ctx.Err() == nil {
			// For example, an old tip was pruned since the last sync.
			logger.Warn("scanning the whole repository for lfs objects, scanning the new history failed", "repo", repo.Name(), "err", err)
			err = storeMissingLFSObjects(ctx, repo, r, dbx, store, lfsClient, lfsRoot, tips, nil)
		}
	}
	if err != nil {
		// Objects that weren't stored stay in the history the next sync
		// scans, as the tips it starts from don't move.
		return err
	}

	if err := writeScannedRefs(markerPath, tips); err != nil {
		// The next sync scans more history than it needs to.
		logger.Warn("failed to save the scanned refs", "repo", repo.Name(), "err", err)
	}
	return nil
}

// refTips returns the sorted, distinct object ids the refs and HEAD of r
// point to, the tips git rev-list --all starts from.
func refTips(ctx context.Context, r *git.Repository) ([]string, error) {
	out, err := gitm.NewCommandWithContext(ctx, "for-each-ref", "--format=%(objectname)").RunInDir(r.Path)
	if err != nil {
		return nil, fmt.Errorf("listing refs: %w", err)
	}
	tips := strings.Fields(string(out))
	// A detached HEAD, which git clone --mirror copies, can reach commits
	// no ref does. An unborn HEAD doesn't resolve and adds nothing.
	if head, err := gitm.NewCommandWithContext(ctx, "rev-parse", "-q", "--verify", "HEAD").RunInDir(r.Path); err == nil {
		if id := strings.TrimSpace(string(head)); isObjectID(id) {
			tips = append(tips, id)
		}
	}
	slices.Sort(tips)
	return slices.Compact(tips), nil
}

// readScannedRefs returns the tips saved in the file at path, sorted and
// distinct, or nil if there is no usable file.
func readScannedRefs(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	tips := []string{}
	for _, id := range strings.Fields(string(data)) {
		if !isObjectID(id) {
			return nil
		}
		tips = append(tips, id)
	}
	slices.Sort(tips)
	return slices.Compact(tips)
}

// writeScannedRefs saves tips to the file at path, replacing it at once so
// a sync never reads half of it.
func writeScannedRefs(path string, tips []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), lfsScannedRefsFile+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) //nolint: errcheck
	var data strings.Builder
	for _, id := range tips {
		data.WriteString(id + "\n")
	}
	if _, err := f.WriteString(data.String()); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// isObjectID reports whether s is a sha1 or sha256 object id.
func isObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// storeMissingLFSObjects stores the missing LFS objects of the pointers
// reachable from include but not from exclude. Errors of the scan itself are
// returned as an *lfsScanError.
func storeMissingLFSObjects(ctx context.Context, repo proto.Repository, r *git.Repository, dbx *db.DB, store store.Store, lfsClient lfs.Client, lfsRoot string, include, exclude []string) error {
	// TODO: support S3 storage
	strg := storage.NewLocalStorage(lfsRoot)

	// Cancel the scan when we return early so its goroutines and git
	// processes don't stay blocked on pointerChan.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	pointerChan := make(chan lfs.PointerBlob)
	errChan := make(chan error, 1)
	go searchPointerBlobs(ctx, r, include, exclude, pointerChan, errChan)

	// recorded holds the objects that are already in the database but missing
	// on disk. Those only need their content written.
	recorded := make(map[string]bool)
	logger := log.FromContext(ctx).WithPrefix("lfs")
	var unavailable int
	objectFailed := func(p lfs.Pointer, err error) {
		logger.Warn("skipping lfs object", "repo", repo.Name(), "oid", p.Oid, "err", err)
		unavailable++
	}

	download := func(pointers []lfs.Pointer) error {
		pending := make(map[string]lfs.Pointer, len(pointers))
		for _, p := range pointers {
			pending[p.Oid] = p
		}

		if err := lfsClient.Download(ctx, pointers, func(got lfs.Pointer, content io.ReadCloser, objectError error) error {
			if content != nil {
				defer content.Close() //nolint: errcheck
			}

			// The pointer comes from the remote's response. Only take
			// objects we asked for, and go by our own pointer from then on.
			p, ok := pending[got.Oid]
			if !got.IsValid() || !ok {
				logger.Warn("ignoring lfs object that was not requested", "repo", repo.Name(), "oid", got.Oid)
				return nil
			}
			delete(pending, got.Oid)

			if objectError != nil {
				objectFailed(p, objectError)
				return nil
			}

			// Stream the object to a temporary file outside of any database
			// transaction, so a slow download doesn't hold the write lock.
			randBytes := make([]byte, 12)
			if _, err := rand.Read(randBytes); err != nil {
				return err
			}

			tempName := path.Join("incomplete", fmt.Sprintf("%s%x", p.Oid, randBytes))
			// Read one byte past the expected size, enough to tell that
			// the content is too long without storing all of it.
			hash := sha256.New()
			written, err := strg.Put(tempName, io.TeeReader(io.LimitReader(content, p.Size+1), hash))
			if err != nil {
				_ = strg.Delete(tempName)
				return err
			}

			if sum := hex.EncodeToString(hash.Sum(nil)); written != p.Size || sum != p.Oid {
				_ = strg.Delete(tempName)
				objectFailed(p, fmt.Errorf("content does not match: got %d bytes with sha256 %s", written, sum))
				return nil
			}

			if err := strg.Rename(tempName, path.Join("objects", p.RelativePath())); err != nil {
				_ = strg.Delete(tempName)
				return err
			}

			if recorded[p.Oid] {
				return nil
			}

			// If recording fails, the object stays on disk and the next sync
			// records it.
			return recordLFSObject(ctx, dbx, store, repo.ID(), p)
		}); err != nil {
			return err
		}

		for _, p := range pending {
			objectFailed(p, errors.New("not returned by the remote"))
		}
		return nil
	}

	var batch []lfs.Pointer
	// queued holds the oids already batched. Different pointer blobs can
	// share an oid, and the download callback expects each oid once.
	queued := make(map[string]bool)
	for pointer := range pointerChan {
		if queued[pointer.Oid] {
			continue
		}

		obj, err := store.GetLFSObjectByOid(ctx, dbx, repo.ID(), pointer.Oid)
		if err != nil && !errors.Is(err, db.ErrRecordNotFound) {
			return db.WrapError(err)
		}

		// A file of the wrong size, such as one an interrupted download left
		// behind, counts as missing and is downloaded again.
		var exist bool
		fi, err := strg.Stat(path.Join("objects", pointer.RelativePath()))
		switch {
		case err == nil:
			exist = fi.Size() == pointer.Size
		case !errors.Is(err, fs.ErrNotExist):
			return err
		}

		if exist {
			// The object is on disk, make sure it's recorded.
			if obj.ID == 0 {
				if err := recordLFSObject(ctx, dbx, store, repo.ID(), pointer.Pointer); err != nil {
					return err
				}
			}
			continue
		}

		if obj.ID != 0 {
			recorded[pointer.Oid] = true
		}

		queued[pointer.Oid] = true
		batch = append(batch, pointer.Pointer)
		// Limit batch requests to 20 objects
		if len(batch) >= 20 {
			if err := download(batch); err != nil {
				return err
			}

			batch = nil
		}
	}

	if err, ok := <-errChan; ok {
		return &lfsScanError{err}
	}

	// Download whatever is left over from the last, partial batch.
	if len(batch) > 0 {
		if err := download(batch); err != nil {
			return err
		}
	}

	if unavailable > 0 {
		return &lfsObjectsUnavailableError{count: unavailable}
	}

	return nil
}

// recordLFSObject records an LFS object of a repository. An object someone
// else recorded in the meantime is not an error.
func recordLFSObject(ctx context.Context, dbx *db.DB, store store.Store, repoID int64, p lfs.Pointer) error {
	err := db.WrapError(store.CreateLFSObject(ctx, dbx, repoID, p.Oid, p.Size))
	if errors.Is(err, db.ErrDuplicateKey) {
		// db.WrapError reports foreign key violations, e.g. a repository
		// deleted mid-sync, as duplicate keys too, so check the row is there.
		if _, gerr := store.GetLFSObjectByOid(ctx, dbx, repoID, p.Oid); gerr == nil {
			return nil
		}
	}
	return err
}
