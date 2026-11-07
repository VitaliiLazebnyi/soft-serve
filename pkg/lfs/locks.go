package lfs

import (
	"errors"
	"math"
	"strconv"
)

const (
	// MaxLocksLimit is the largest page of locks returned at once. A larger
	// limit is lowered to it.
	MaxLocksLimit = 100

	// MaxLocksCursor bounds the page number so the row offset,
	// (cursor-1)*limit, stays well within an int on any platform.
	MaxLocksCursor = math.MaxInt32 / MaxLocksLimit
)

// ErrInvalidLocksCursor is returned by ParseLocksCursor for a cursor that is
// not a page number the server hands out.
var ErrInvalidLocksCursor = errors.New("invalid cursor")

// ParseLocksCursor parses the cursor of a lock list or verify request. The
// cursor is a page number between 1 and MaxLocksCursor, as handed out by
// NextLocksCursor; empty means the first page. Anything else is refused with
// ErrInvalidLocksCursor.
func ParseLocksCursor(cursor string) (int, error) {
	if cursor == "" {
		return 1, nil
	}
	page, err := strconv.Atoi(cursor)
	if err != nil || page < 1 || page > MaxLocksCursor {
		return 0, ErrInvalidLocksCursor
	}
	return page, nil
}

// ClampLocksLimit returns the page size to use for a requested limit. Zero or
// less means DefaultLocksLimit, and a limit above MaxLocksLimit is lowered to
// it.
func ClampLocksLimit(limit int) int {
	switch {
	case limit <= 0:
		return DefaultLocksLimit
	case limit > MaxLocksLimit:
		return MaxLocksLimit
	}
	return limit
}

// NextLocksCursor returns the cursor of the page after page, given that n
// locks were returned with the page size limit. It is empty when there is no
// next page: the page was not full, or page is already MaxLocksCursor, past
// which ParseLocksCursor refuses the cursor.
func NextLocksCursor(page, limit, n int) string {
	if n < limit || page >= MaxLocksCursor {
		return ""
	}
	return strconv.Itoa(page + 1)
}
