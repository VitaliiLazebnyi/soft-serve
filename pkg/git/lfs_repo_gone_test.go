package git

import (
	"context"
	"errors"
	"io"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
)

// lockTestUser is a proto.User with just an ID.
type lockTestUser struct {
	proto.User
	id int64
}

func (u lockTestUser) ID() int64 { return u.id }

// A lock taken over SSH for a repository deleted meanwhile fails on the
// lock's foreign key. That must be reported as the repository being gone,
// not as a conflict with an existing lock.
func TestLFSLockOnDeletedRepository(t *testing.T) {
	ctx := context.Background()
	dp := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.DataPath = dp
	ctx = config.WithContext(ctx, cfg)
	dbx, err := db.Open(ctx, "sqlite", dp+"/test.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbx.Close() }) //nolint:errcheck
	if err := migrate.Migrate(ctx, dbx); err != nil {
		t.Fatal(err)
	}
	st := database.New(ctx, dbx)

	admin, err := st.FindUserByUsername(ctx, dbx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRepo(ctx, dbx, "repo1", admin.ID, "", "", false, false, false, false); err != nil {
		t.Fatal(err)
	}
	m, err := st.GetRepoByName(ctx, dbx, "repo1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteRepoByName(ctx, dbx, "repo1"); err != nil {
		t.Fatal(err)
	}

	locks := &lfsLockBackend{
		lfsTransfer: &lfsTransfer{
			ctx:    ctx,
			cfg:    cfg,
			dbx:    dbx,
			store:  st,
			logger: log.New(io.Discard),
			repo:   lfsTestRepo{},
			op:     lfs.OperationUpload,
		},
		user: lockTestUser{id: admin.ID},
	}
	if m.ID != (lfsTestRepo{}).ID() {
		t.Fatalf("repository ID %d, want %d", m.ID, (lfsTestRepo{}).ID())
	}

	if _, err := locks.Create("foo.png", "refs/heads/main"); !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatalf("Create: got %v, want proto.ErrRepoNotFound", err)
	}
}
