package backend

import (
	"errors"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// Postgres runs transactions in READ COMMITTED, so a collaborator added
// after the keep check and before the row is deleted would commit and then
// go with the row through ON DELETE CASCADE, although its caller was told
// it was added. The keep check reads the row with FOR UPDATE, which the
// insert's foreign key check waits for: here the insert must block on the
// deleting transaction (checked with pg_blocking_pids) and then fail,
// since the repository it names is gone.
func TestDeleteRepositoryKeepCheckLocksRowPostgres(t *testing.T) {
	is := is.New(t)
	e := newPostgresWebhookEnv(t)
	_, err := e.be.CreateUser(e.ctx, "alice", proto.UserOptions{})
	is.NoErr(err)

	result := make(chan error, 1)
	err = e.be.deleteRepository(e.ctx, "repo", func(tx *db.Tx, m models.Repo) (bool, error) {
		var holder int
		if err := tx.GetContext(e.ctx, &holder, `SELECT pg_backend_pid();`); err != nil {
			return true, err
		}
		go func() {
			result <- e.be.AddCollaborator(e.ctx, "repo", "alice", access.ReadWriteAccess)
		}()
		waitBlockedBy(t, e, holder, result)
		return false, nil
	})
	is.NoErr(err)

	select {
	case err := <-result:
		is.True(err != nil) // the collaborator was not added to a deleted repository
	case <-time.After(10 * time.Second):
		t.Fatal("AddCollaborator still waiting after the delete committed")
	}
	_, err = e.be.Repository(e.ctx, "repo")
	is.True(errors.Is(err, proto.ErrRepoNotFound))
}
