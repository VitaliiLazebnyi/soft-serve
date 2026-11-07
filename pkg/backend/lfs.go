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

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/storage"
	"github.com/charmbracelet/soft-serve/pkg/store"
)

// StoreRepoMissingLFSObjects stores missing LFS objects for a repository.
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
	download := func(pointers []lfs.Pointer) error {
		return lfsClient.Download(ctx, pointers, func(p lfs.Pointer, content io.ReadCloser, objectError error) error {
			if objectError != nil {
				return objectError
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
			if err := store.CreateLFSObject(ctx, dbx, repo.ID(), p.Oid, p.Size); err != nil {
				// Someone else recorded it in the meantime.
				if err := db.WrapError(err); !errors.Is(err, db.ErrDuplicateKey) {
					return err
				}
			}

			return nil
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
				if err := store.CreateLFSObject(ctx, dbx, repo.ID(), pointer.Oid, pointer.Size); err != nil {
					// Someone else recorded it in the meantime.
					if err := db.WrapError(err); !errors.Is(err, db.ErrDuplicateKey) {
						return err
					}
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
		return download(batch)
	}

	return nil
}
