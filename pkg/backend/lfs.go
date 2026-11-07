package backend

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
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

// ErrLFSObjectsUnavailable is returned, joined with the error of each object,
// when the remote could not provide some LFS objects. Every other object was
// still stored.
var ErrLFSObjectsUnavailable = errors.New("some lfs objects are unavailable")

// StoreRepoMissingLFSObjects stores missing LFS objects for a repository.
//
// Objects the remote can't provide are logged and skipped, and reported
// together as ErrLFSObjectsUnavailable once every other object is stored.
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
	var unavailable []error
	objectFailed := func(p lfs.Pointer, err error) {
		logger.Warn("skipping lfs object", "repo", repo.Name(), "oid", p.Oid, "err", err)
		unavailable = append(unavailable, fmt.Errorf("lfs object %s: %w", p.Oid, err))
	}

	download := func(pointers []lfs.Pointer) error {
		return lfsClient.Download(ctx, pointers, func(p lfs.Pointer, content io.ReadCloser, objectError error) error {
			if objectError != nil {
				objectFailed(p, objectError)
				return nil
			}

			defer content.Close() //nolint: errcheck

			// Stream the object to a temporary file outside of any database
			// transaction, so a slow download doesn't hold the write lock.
			randBytes := make([]byte, 12)
			if _, err := rand.Read(randBytes); err != nil {
				return err
			}

			tempName := path.Join("incomplete", fmt.Sprintf("%s%x", p.Oid, randBytes))
			if _, err := strg.Put(tempName, content); err != nil {
				_ = strg.Delete(tempName)
				return err
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
		})
	}

	var batch []lfs.Pointer
	for pointer := range pointerChan {
		obj, err := store.GetLFSObjectByOid(ctx, dbx, repo.ID(), pointer.Oid)
		if err != nil && !errors.Is(err, db.ErrRecordNotFound) {
			return db.WrapError(err)
		}

		exist, err := strg.Exists(path.Join("objects", pointer.RelativePath()))
		if err != nil {
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
		return err
	}

	// Download whatever is left over from the last, partial batch.
	if len(batch) > 0 {
		if err := download(batch); err != nil {
			return err
		}
	}

	if len(unavailable) > 0 {
		return fmt.Errorf("%w: %w", ErrLFSObjectsUnavailable, errors.Join(unavailable...))
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
