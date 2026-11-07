package store

import (
	"context"
	"errors"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// RepoGoneError returns proto.ErrRepoNotFound if the repository with the ID
// repoID no longer exists, and err otherwise. It is for the error of an
// insert of a row that belongs to that repository, such as a collaborator,
// webhook or LFS lock or object. An insert that loses a race with the
// repository's delete fails on a constraint: a foreign key, which
// db.WrapError reports as db.ErrDuplicateKey, or the repository's ID looked
// up by name coming back NULL. Either would otherwise read as a duplicate or
// a database failure.
//
// h must not be a transaction the failed statement ran in: Postgres has
// aborted it.
func RepoGoneError(ctx context.Context, s RepositoryStore, h db.Handler, repoID int64, err error) error {
	if err == nil {
		return nil
	}
	if _, gerr := s.GetRepoByID(ctx, h, repoID); errors.Is(db.WrapError(gerr), db.ErrRecordNotFound) {
		return proto.ErrRepoNotFound
	}
	return err
}
