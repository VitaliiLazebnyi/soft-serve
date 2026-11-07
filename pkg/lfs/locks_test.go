package lfs

import (
	"strconv"
	"testing"
)

func TestParseLocksCursor(t *testing.T) {
	for cursor, want := range map[string]int{
		"":                               1,
		"1":                              1,
		"01":                             1,
		strconv.Itoa(MaxLocksCursor):     MaxLocksCursor,
		"0":                              0,
		"-1":                             0,
		"abc":                            0,
		strconv.Itoa(MaxLocksCursor + 1): 0,
		"9223372036854775807":            0,
		"99999999999999999999":           0,
	} {
		page, err := ParseLocksCursor(cursor)
		if want == 0 {
			if err != ErrInvalidLocksCursor {
				t.Errorf("ParseLocksCursor(%q) = %d, %v; want %v", cursor, page, err, ErrInvalidLocksCursor)
			}
			continue
		}
		if err != nil || page != want {
			t.Errorf("ParseLocksCursor(%q) = %d, %v; want %d", cursor, page, err, want)
		}
	}
}

func TestClampLocksLimit(t *testing.T) {
	for limit, want := range map[int]int{
		-1:                DefaultLocksLimit,
		0:                 DefaultLocksLimit,
		1:                 1,
		MaxLocksLimit:     MaxLocksLimit,
		MaxLocksLimit + 1: MaxLocksLimit,
	} {
		if got := ClampLocksLimit(limit); got != want {
			t.Errorf("ClampLocksLimit(%d) = %d, want %d", limit, got, want)
		}
	}
}

// TestNextLocksCursor checks that every cursor handed out is one
// ParseLocksCursor accepts, including after a full page at the largest one.
func TestNextLocksCursor(t *testing.T) {
	for _, tc := range []struct {
		page, limit, n int
		want           string
	}{
		{1, 2, 2, "2"},
		{1, 2, 1, ""},
		{1, 2, 0, ""},
		{MaxLocksCursor - 1, 1, 1, strconv.Itoa(MaxLocksCursor)},
		{MaxLocksCursor, 1, 1, ""},
	} {
		got := NextLocksCursor(tc.page, tc.limit, tc.n)
		if got != tc.want {
			t.Errorf("NextLocksCursor(%d, %d, %d) = %q, want %q", tc.page, tc.limit, tc.n, got, tc.want)
		}
		if got != "" {
			if _, err := ParseLocksCursor(got); err != nil {
				t.Errorf("NextLocksCursor(%d, %d, %d) = %q, which ParseLocksCursor refuses: %v", tc.page, tc.limit, tc.n, got, err)
			}
		}
	}
}
