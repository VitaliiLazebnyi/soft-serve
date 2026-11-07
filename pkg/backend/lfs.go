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
	"path"
	"path/filepath"
	"strconv"

	"charm.land/log/v2"
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

// StoreRepoMissingLFSObjects stores missing LFS objects for a repository.
//
// Objects the remote can't provide are logged and skipped, and counted in an
// error wrapping ErrLFSObjectsUnavailable once every other object is stored.
// Any other error stops the sync.
func StoreRepoMissingLFSObjects(ctx context.Context, repo proto.Repository, dbx *db.DB, store store.Store, lfsClient lfs.Client) error {
	cfg := config.FromContext(ctx)
	repoID := strconv.FormatInt(repo.ID(), 10)
	lfsRoot := filepath.Join(cfg.DataPath, "lfs", repoID)

	// TODO: support S3 storage
	strg := storage.NewLocalStorage(lfsRoot)
	r, err := repo.Open()
	if err != nil {
		return err
	}

	// Cancel the scan when we return early so its goroutines and git
	// processes don't stay blocked on pointerChan.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	pointerChan := make(chan lfs.PointerBlob)
	errChan := make(chan error, 1)
	go lfs.SearchPointerBlobs(ctx, r, pointerChan, errChan)

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

	// missing reports whether the object of pointer has to be downloaded.
	// An object already on disk is recorded if it isn't yet.
	missing := func(pointer lfs.Pointer) (bool, error) {
		obj, err := store.GetLFSObjectByOid(ctx, dbx, repo.ID(), pointer.Oid)
		if err != nil && !errors.Is(err, db.ErrRecordNotFound) {
			return false, db.WrapError(err)
		}

		// A file of the wrong size, such as one an interrupted download left
		// behind, counts as missing and is downloaded again.
		fi, err := strg.Stat(path.Join("objects", pointer.RelativePath()))
		switch {
		case err == nil && fi.Size() == pointer.Size:
			// The object is on disk, make sure it's recorded.
			if obj.ID == 0 {
				if err := recordLFSObject(ctx, dbx, store, repo.ID(), pointer); err != nil {
					return false, err
				}
			}
			return false, nil
		case err == nil && obj.ID != 0 && obj.Size == fi.Size():
			// The object is stored and recorded with another size, which
			// a pointer with this oid can't have, so this pointer is
			// malformed. Downloading it would only fail its verification
			// on every sync.
			logger.Debug("ignoring lfs pointer with the wrong size", "repo", repo.Name(), "oid", pointer.Oid, "size", pointer.Size, "stored", obj.Size)
			return false, nil
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return false, err
		}

		if obj.ID != 0 {
			recorded[pointer.Oid] = true
		}
		return true, nil
	}

	// Different pointer blobs can share an oid and size, for example when
	// one has a trailing line, so pointers are queued once per oid and size.
	// Pointers can also share an oid but not the size, when one of them is
	// malformed; each one is tried, as the first one scanned may be the bad
	// one.
	type pointerKey struct {
		oid  string
		size int64
	}
	queued := make(map[pointerKey]bool)

	// The download callback expects each oid once per batch, so a pointer
	// whose oid is already in the batch waits in deferred for a later one.
	var batch, deferred []lfs.Pointer
	batched := make(map[string]bool)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := download(batch); err != nil {
			return err
		}
		batch = nil
		clear(batched)
		return nil
	}
	queue := func(pointer lfs.Pointer) error {
		if batched[pointer.Oid] {
			deferred = append(deferred, pointer)
			return nil
		}
		batched[pointer.Oid] = true
		batch = append(batch, pointer)
		// Limit batch requests to 20 objects
		if len(batch) >= 20 {
			return flush()
		}
		return nil
	}

	for pointer := range pointerChan {
		key := pointerKey{pointer.Oid, pointer.Size}
		if queued[key] {
			continue
		}

		need, err := missing(pointer.Pointer)
		if err != nil {
			return err
		}
		if !need {
			continue
		}

		queued[key] = true
		if err := queue(pointer.Pointer); err != nil {
			return err
		}
	}

	if err, ok := <-errChan; ok {
		return err
	}

	// Download whatever is left over from the last, partial batch, then the
	// pointers that shared an oid with another one in their batch. Those
	// are checked again, as the other pointer may have stored the object.
	for len(batch) > 0 || len(deferred) > 0 {
		if err := flush(); err != nil {
			return err
		}

		pointers := deferred
		deferred = nil
		for _, pointer := range pointers {
			need, err := missing(pointer)
			if err != nil {
				return err
			}
			if need {
				if err := queue(pointer); err != nil {
					return err
				}
			}
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
