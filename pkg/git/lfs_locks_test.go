package git

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/charmbracelet/git-lfs-transfer/transfer"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
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
		strconv.Itoa(maxLocksCursor + 1),
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
