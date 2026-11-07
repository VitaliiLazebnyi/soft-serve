package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// newLocksParamsContext returns a request context for a user who owns a
// repository holding two locks.
func newLocksParamsContext(t *testing.T) context.Context {
	t.Helper()
	is := is.New(t)
	ctx, be, datastore := newLFSTestContext(t)
	dbx := db.FromContext(ctx)

	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	is.NoErr(err)
	repo, err := be.CreateRepository(ctx, "repo", owner, proto.RepositoryOptions{})
	is.NoErr(err)
	is.NoErr(datastore.CreateLFSLockForUser(ctx, dbx, repo.ID(), owner.ID(), "a.bin", "refs/heads/main"))
	is.NoErr(datastore.CreateLFSLockForUser(ctx, dbx, repo.ID(), owner.ID(), "b.bin", "refs/heads/main"))

	ctx = proto.WithUserContext(ctx, owner)
	return proto.WithRepositoryContext(ctx, repo)
}

// TestLFSLocksGetRejectsInvalidParams checks that the lock list rejects an
// id, cursor or limit it cannot use. These used to be parsed with their
// errors ignored, so ?id=abc read as no id and listed every lock instead
// of looking one up.
func TestLFSLocksGetRejectsInvalidParams(t *testing.T) {
	ctx := newLocksParamsContext(t)

	for _, query := range []string{
		"id=abc",
		"id=0",
		"id=-1",
		"id=99999999999999999999",
		"cursor=abc",
		"cursor=0",
		"cursor=-1",
		"cursor=9223372036854775807",
		"limit=abc",
		"limit=-1",
		"limit=99999999999999999999",
	} {
		t.Run(query, func(t *testing.T) {
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/repo.git/info/lfs/locks?"+query, nil)
			req.Header.Set("Accept", lfs.MediaType)
			w := httptest.NewRecorder()
			serviceLfsLocks(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusBadRequest, w.Body)
			}
			var resp lfs.ErrorResponse
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil || resp.Message == "" {
				t.Errorf("body is not an LFS error (%v): %+v", err, resp)
			}
		})
	}
}

// TestLFSLocksGetValidParams checks that valid paging still works, and that
// a limit above the maximum is lowered rather than rejected.
func TestLFSLocksGetValidParams(t *testing.T) {
	ctx := newLocksParamsContext(t)

	for query, want := range map[string]struct {
		locks  int
		cursor string
	}{
		"":                  {locks: 2},
		"limit=1":           {locks: 1, cursor: "2"},
		"cursor=2&limit=1":  {locks: 1, cursor: "3"},
		"cursor=3&limit=1":  {locks: 0},
		"limit=1000":        {locks: 2},
		"cursor=1&limit=0":  {locks: 2},
		"cursor=21474836":   {locks: 0},
		"path=a.bin":        {locks: 1},
		"path=a.bin&id=":    {locks: 1},
		"cursor=&limit=":    {locks: 2},
		"limit=2&cursor=01": {locks: 2, cursor: "2"},
	} {
		t.Run(query, func(t *testing.T) {
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/repo.git/info/lfs/locks?"+query, nil)
			req.Header.Set("Accept", lfs.MediaType)
			w := httptest.NewRecorder()
			serviceLfsLocks(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body)
			}
			var resp lfs.LockListResponse
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(resp.Locks) != want.locks || resp.NextCursor != want.cursor {
				t.Errorf("got %d locks, next_cursor %q; want %d, %q", len(resp.Locks), resp.NextCursor, want.locks, want.cursor)
			}
		})
	}
}

// TestLFSLocksVerifyRejectsInvalidParams covers the same cursor and limit
// checks on the verify endpoint, where an invalid cursor used to be read as
// the first page.
func TestLFSLocksVerifyRejectsInvalidParams(t *testing.T) {
	ctx := newLocksParamsContext(t)

	verify := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/repo.git/info/lfs/locks/verify", strings.NewReader(body))
		req.Header.Set("Content-Type", lfs.MediaType)
		req.Header.Set("Accept", lfs.MediaType)
		w := httptest.NewRecorder()
		serviceLfsLocksVerify(w, req)
		return w
	}

	for _, body := range []string{
		`{"cursor":"abc"}`,
		`{"cursor":"0"}`,
		`{"cursor":"-1"}`,
		`{"cursor":"9223372036854775807"}`,
		`{"limit":-1}`,
	} {
		t.Run(body, func(t *testing.T) {
			w := verify(body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusBadRequest, w.Body)
			}
			var resp lfs.ErrorResponse
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil || resp.Message == "" {
				t.Errorf("body is not an LFS error (%v): %+v", err, resp)
			}
		})
	}

	w := verify(`{"cursor":"2","limit":1}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body)
	}
	var resp lfs.LockVerifyResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Ours) != 1 || len(resp.Theirs) != 0 || resp.NextCursor != "3" {
		t.Errorf("got ours %d, theirs %d, next_cursor %q; want 1, 0, \"3\"", len(resp.Ours), len(resp.Theirs), resp.NextCursor)
	}
}
