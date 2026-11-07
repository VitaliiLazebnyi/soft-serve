package store

import (
	"context"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
)

// RepositoryStore is an interface for managing repositories.
type RepositoryStore interface {
	GetRepoByName(ctx context.Context, h db.Handler, name string) (models.Repo, error)
	// GetRepoByNameForUpdate is GetRepoByName that also locks the row until
	// the transaction ends, on Postgres, with FOR UPDATE: the only row lock
	// that also holds off the key-share lock a foreign key check takes, so
	// no collaborator, webhook or LFS row can be added for the repository
	// meanwhile. On SQLite a writing transaction already excludes other
	// writers, and it is GetRepoByName.
	GetRepoByNameForUpdate(ctx context.Context, h db.Handler, name string) (models.Repo, error)
	GetAllRepos(ctx context.Context, h db.Handler) ([]models.Repo, error)
	GetUserRepos(ctx context.Context, h db.Handler, userID int64) ([]models.Repo, error)
	CreateRepo(ctx context.Context, h db.Handler, name string, userID int64, projectName string, description string, isPrivate bool, isHidden bool, isMirror bool, isAnonymous bool) error
	// CountUserRepos counts the repositories userID created, leaving out
	// those created anonymously that userID owns as the default admin.
	CountUserRepos(ctx context.Context, h db.Handler, userID int64) (int64, error)
	// CountAnonymousRepos counts the repositories created without an account.
	CountAnonymousRepos(ctx context.Context, h db.Handler) (int64, error)
	// LockUserRepos locks userID's row until the transaction ends, on
	// Postgres, so that concurrent callers that insert a repository owned by
	// userID and then count take turns. On SQLite callers take turns because
	// the insert already holds the write lock, and it does nothing.
	LockUserRepos(ctx context.Context, h db.Handler, userID int64) error
	DeleteRepoByName(ctx context.Context, h db.Handler, name string) error
	SetRepoNameByName(ctx context.Context, h db.Handler, name string, newName string) error

	GetRepoProjectNameByName(ctx context.Context, h db.Handler, name string) (string, error)
	SetRepoProjectNameByName(ctx context.Context, h db.Handler, name string, projectName string) error
	GetRepoDescriptionByName(ctx context.Context, h db.Handler, name string) (string, error)
	SetRepoDescriptionByName(ctx context.Context, h db.Handler, name string, description string) error
	GetRepoIsPrivateByName(ctx context.Context, h db.Handler, name string) (bool, error)
	SetRepoIsPrivateByName(ctx context.Context, h db.Handler, name string, isPrivate bool) error
	GetRepoIsHiddenByName(ctx context.Context, h db.Handler, name string) (bool, error)
	SetRepoIsHiddenByName(ctx context.Context, h db.Handler, name string, isHidden bool) error
	GetRepoIsMirrorByName(ctx context.Context, h db.Handler, name string) (bool, error)
	SetRepoIsMirrorByName(ctx context.Context, h db.Handler, name string, isMirror bool) error
}
