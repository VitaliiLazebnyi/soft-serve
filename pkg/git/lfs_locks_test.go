package git

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/git-lfs-transfer/transfer"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/store"
)

// TestLFSLockRangeRejectsInvalidCursor checks that listing locks over SSH
// refuses a cursor that is not a page number it hands out. A cursor that did
// not parse used to read as the first page, and a huge one overflowed the
// store's row offset, (cursor-1)*limit. The backend has no database on
// purpose: a refused cursor must not reach it.
func TestLFSLockRangeRejectsInvalidCursor(t *testing.T) {
	l := &lfsLockBackend{
		lfsTransfer: &lfsTransfer{ctx: context.Background(), op: lfs.OperationUpload},
		user:        lfsTestUser{},
	}

	for _, cursor := range []string{
		"abc",
		"1x",
		"0",
		"-1",
		strconv.Itoa(lfs.MaxLocksCursor + 1),
		"9223372036854775807",
		"99999999999999999999",
	} {
		t.Run(cursor, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Range(%q) reached the database: %v", cursor, r)
				}
			}()
			_, err := l.Range(cursor, 10, func(transfer.Lock) error { return nil })
			if !errors.Is(err, transfer.ErrParseError) {
				t.Errorf("Range(%q) error = %v, want %v", cursor, err, transfer.ErrParseError)
			}
		})
	}
}

// fullPageLockStore returns a full page of locks for any page, standing in
// for a repository with more locks than the last page can reach.
type fullPageLockStore struct{ store.Store }

func (fullPageLockStore) GetLFSLocks(_ context.Context, _ db.Handler, repoID int64, page int, limit int) ([]models.LFSLock, error) {
	locks := make([]models.LFSLock, limit)
	for i := range locks {
		locks[i] = models.LFSLock{ID: int64(i + 1), RepoID: repoID, UserID: 1, Path: strconv.Itoa(i)}
	}
	return locks, nil
}

func (fullPageLockStore) GetUserByID(_ context.Context, _ db.Handler, id int64) (models.User, error) {
	return models.User{ID: id, Username: "owner"}, nil
}

// TestLFSLockRangeLastPage checks that a full page at the largest cursor the
// server accepts hands out no next cursor. It used to hand out
// lfs.MaxLocksCursor+1, which the next request was then refused for.
func TestLFSLockRangeLastPage(t *testing.T) {
	dbx, err := db.Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { dbx.Close() }) //nolint:errcheck

	l := &lfsLockBackend{
		lfsTransfer: &lfsTransfer{
			ctx:    context.Background(),
			dbx:    dbx,
			store:  fullPageLockStore{},
			logger: log.New(io.Discard),
			repo:   lfsTestRepo{},
			op:     lfs.OperationUpload,
		},
		user: lfsTestUser{},
	}

	for cursor, want := range map[string]string{
		strconv.Itoa(lfs.MaxLocksCursor - 1): strconv.Itoa(lfs.MaxLocksCursor),
		strconv.Itoa(lfs.MaxLocksCursor):     "",
	} {
		var n int
		next, err := l.Range(cursor, 1, func(transfer.Lock) error { n++; return nil })
		if err != nil {
			t.Fatalf("Range(%q): %v", cursor, err)
		}
		if n != 1 || next != want {
			t.Errorf("Range(%q) = %d locks, next cursor %q; want 1, %q", cursor, n, next, want)
		}
	}
}
