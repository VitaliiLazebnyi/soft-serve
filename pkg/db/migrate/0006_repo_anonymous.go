package migrate

import (
	"context"

	"github.com/charmbracelet/soft-serve/pkg/db"
)

const (
	repoAnonymousName    = "repo_anonymous"
	repoAnonymousVersion = 6
)

// Mark repositories created without an account. They are owned by the
// default admin (repos.user_id is NOT NULL), so the owner alone cannot tell
// them apart from the admin's own; max_repos_per_user counts them together
// as one user. Repositories that existed before this migration are not
// marked, since nothing records how they were created.
var repoAnonymous = Migration{
	Name:    repoAnonymousName,
	Version: repoAnonymousVersion,
	Migrate: func(ctx context.Context, tx *db.Tx) error {
		return migrateUp(ctx, tx, repoAnonymousVersion, repoAnonymousName)
	},
	Rollback: func(ctx context.Context, tx *db.Tx) error {
		return migrateDown(ctx, tx, repoAnonymousVersion, repoAnonymousName)
	},
}
