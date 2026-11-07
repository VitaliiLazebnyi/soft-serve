package lfs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/storage"
	"github.com/charmbracelet/soft-serve/pkg/store"
)

var (
	// ErrObjectSizeMismatch is returned by StoreObject when the data is not
	// the declared size.
	ErrObjectSizeMismatch = errors.New("object size does not match")

	// ErrObjectOidMismatch is returned by StoreObject when the SHA-256 of
	// the data is not the object ID.
	ErrObjectOidMismatch = errors.New("object content does not match oid")
)

// StoreObject verifies an uploaded object and stores it in strg, then records
// it in datastore for the repository.
//
// The data is written to a temporary file under incomplete/ while it is
// hashed, reading at most one byte past size so an oversized object is not
// written out in full. Only if exactly size bytes were read and their SHA-256
// is oid is the file renamed into objects/ and recorded. A concurrent upload
// of the same object renames identical bytes over the same path. If the
// object is already recorded, by an earlier or concurrent upload, that counts
// as success, but only once the row reads back: db.WrapError maps foreign key
// violations to db.ErrDuplicateKey too. The temporary file is always removed.
//
// A mismatch is reported as ErrObjectSizeMismatch or ErrObjectOidMismatch;
// any other error is a storage or database failure. size must not be
// negative. StoreObject does not read r past size+1 bytes; draining the rest
// is up to the caller.
func StoreObject(ctx context.Context, strg storage.Storage, dbx db.Handler, datastore store.LFSStore, repoID int64, oid string, size int64, r io.Reader) error {
	randBytes := make([]byte, 12)
	if _, err := rand.Read(randBytes); err != nil {
		return fmt.Errorf("generate temp name: %w", err)
	}
	tempName := path.Join("incomplete", fmt.Sprintf("%s%x", oid, randBytes))
	defer strg.Delete(tempName) //nolint: errcheck

	hash := sha256.New()
	written, err := strg.Put(tempName, io.TeeReader(io.LimitReader(r, size+1), hash))
	if err != nil {
		return fmt.Errorf("write object: %w", err)
	}
	if written != size {
		return fmt.Errorf("%w: expected %d bytes, got at least %d", ErrObjectSizeMismatch, size, written)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != oid {
		return fmt.Errorf("%w: expected %s, got %s", ErrObjectOidMismatch, oid, got)
	}

	// Rename takes names relative to the storage root, not the absolute path
	// the temp file happens to live at.
	pointer := Pointer{Oid: oid, Size: size}
	if err := strg.Rename(tempName, path.Join("objects", pointer.RelativePath())); err != nil {
		return fmt.Errorf("rename object: %w", err)
	}

	if err := datastore.CreateLFSObject(ctx, dbx, repoID, oid, size); err != nil {
		if !errors.Is(err, db.ErrDuplicateKey) {
			return fmt.Errorf("record object: %w", err)
		}
		if _, gerr := datastore.GetLFSObjectByOid(ctx, dbx, repoID, oid); gerr != nil {
			return fmt.Errorf("record object: %w (lookup: %v)", err, gerr)
		}
	}

	return nil
}
