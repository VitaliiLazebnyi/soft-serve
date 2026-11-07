package migrate

import (
	"context"

	"github.com/charmbracelet/soft-serve/pkg/db"
)

const (
	lfsObjectSizeBigintName    = "lfs_object_size_bigint"
	lfsObjectSizeBigintVersion = 7
)

// Widen lfs_objects.size to 64 bits. Postgres created it as INTEGER, which
// holds at most 2^31-1, so recording an LFS object of 2 GiB or more failed.
// SQLite's INTEGER is already 64-bit, so its migration does nothing.
var lfsObjectSizeBigint = Migration{
	Name:    lfsObjectSizeBigintName,
	Version: lfsObjectSizeBigintVersion,
	Migrate: func(ctx context.Context, tx *db.Tx) error {
		return migrateUp(ctx, tx, lfsObjectSizeBigintVersion, lfsObjectSizeBigintName)
	},
	Rollback: func(ctx context.Context, tx *db.Tx) error {
		return migrateDown(ctx, tx, lfsObjectSizeBigintVersion, lfsObjectSizeBigintName)
	},
}
