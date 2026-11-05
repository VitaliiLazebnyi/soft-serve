package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
)

// defaultAdminUsername is the admin account the first migration creates.
const defaultAdminUsername = "admin"

// syncAdminKeys makes sure every key in initial_admin_keys is in the
// database. The first migration seeds the keys once; without this, a key
// added to the config later never reaches the database, and anything that
// checks only the account's admin flag ignores it.
//
// It runs on every Migrate, after the migrations and in the same
// transaction, and is idempotent:
//   - a key no account owns is attached to the admin account: the admin with
//     the lowest id, normally the one the first migration created. If there
//     is no admin at all, an "admin" account is created as the migration does;
//   - a key an admin already owns is left alone;
//   - a key a non-admin account owns is left alone with a warning: the
//     account is not promoted and the key is not moved;
//   - keys removed from the config are never removed from the database, and
//     admin rights are never revoked.
func syncAdminKeys(ctx context.Context, tx *db.Tx) error {
	cfg := config.FromContext(ctx)
	if cfg == nil {
		return nil
	}
	keys := cfg.AdminKeys()
	if len(keys) == 0 {
		return nil
	}

	logger := log.FromContext(ctx).WithPrefix("migrate")

	var adminID int64
	var haveAdmin bool
	for _, k := range keys {
		ak := sshutils.MarshalAuthorizedKey(k)

		var owner struct {
			Username string `db:"username"`
			Admin    bool   `db:"admin"`
		}
		err := tx.GetContext(ctx, &owner, tx.Rebind(`SELECT users.username, users.admin
			FROM users
			INNER JOIN public_keys ON users.id = public_keys.user_id
			WHERE public_keys.public_key = ?`), ak)
		if err == nil {
			if !owner.Admin {
				logger.Warn("initial admin key belongs to a non-admin user; not promoting the user",
					"user", owner.Username, "key", ak)
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("look up owner of initial admin key: %w", err)
		}

		if !haveAdmin {
			id, ok, err := adminUserID(ctx, tx)
			if err != nil {
				return err
			}
			if !ok {
				logger.Warn("no admin user to attach initial admin keys to, and the username is taken by a non-admin user",
					"user", defaultAdminUsername)
				return nil
			}
			adminID, haveAdmin = id, true
		}

		if err := insertPublicKey(ctx, tx, adminID, ak); err != nil {
			return fmt.Errorf("add initial admin key: %w", err)
		}
		logger.Debug("initial admin key attached to admin user", "key", ak)
	}

	return nil
}

// adminUserID returns the id of the admin account initial admin keys go to,
// creating the default admin account when there is no admin at all. ok is
// false when there is no admin and the default username belongs to a
// non-admin account, which is never promoted.
func adminUserID(ctx context.Context, tx *db.Tx) (id int64, ok bool, err error) {
	err = tx.GetContext(ctx, &id, tx.Rebind(`SELECT id FROM users WHERE admin = ? ORDER BY id LIMIT 1`), true)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("look up admin user: %w", err)
	}

	insert := insertIgnore(tx, `INTO users (username, admin, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)`)
	if _, err := tx.ExecContext(ctx, insert, defaultAdminUsername, true); err != nil {
		return 0, false, fmt.Errorf("create admin user: %w", err)
	}

	err = tx.GetContext(ctx, &id, tx.Rebind(`SELECT id FROM users WHERE username = ? AND admin = ?`), defaultAdminUsername, true)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("look up admin user: %w", err)
	}
	return id, true, nil
}

// insertPublicKey attaches ak to userID. A key that a concurrent boot
// inserted in the meantime is not an error.
func insertPublicKey(ctx context.Context, tx *db.Tx, userID int64, ak string) error {
	query := insertIgnore(tx, `INTO public_keys (user_id, public_key, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)`)
	if _, err := tx.ExecContext(ctx, query, userID, ak); err != nil {
		if errors.Is(db.WrapError(err), db.ErrDuplicateKey) {
			return nil
		}
		return err
	}
	return nil
}

// insertIgnore builds an INSERT that does nothing on a unique conflict, for
// the transaction's driver. rest starts at "INTO".
func insertIgnore(tx *db.Tx, rest string) string {
	switch tx.DriverName() {
	case "sqlite3", "sqlite":
		return tx.Rebind("INSERT OR IGNORE " + rest)
	case "postgres":
		return tx.Rebind("INSERT " + rest + " ON CONFLICT DO NOTHING")
	default:
		return tx.Rebind("INSERT " + rest)
	}
}
