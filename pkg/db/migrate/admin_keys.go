package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	gossh "golang.org/x/crypto/ssh"
)

// defaultAdminUsername is the admin account the first migration creates.
const defaultAdminUsername = "admin"

// syncedKeyPrefix prefixes the settings rows that record which keys
// syncAdminKeys attached. See db.SyncedAdminKeyPrefix.
const syncedKeyPrefix = db.SyncedAdminKeyPrefix

// syncAdminKeys makes sure every key in initial_admin_keys is in the
// database. The first migration seeds the keys once; without this, a key
// added to the config later never reaches the database, and anything that
// checks only the account's admin flag ignores it.
//
// It runs on every Migrate, after the migrations and in the same
// transaction, and is idempotent:
//   - a key no account owns is attached to an admin account: the admin that
//     owns another configured key, else the admin named "admin", else the
//     admin with the lowest id. If there is no admin at all, an "admin"
//     account is created as the first migration does;
//   - a key an admin already owns is left alone;
//   - a key a non-admin account owns is left alone with a warning: the
//     account is not promoted and the key is not moved;
//   - a key this function attached is removed again once it leaves the
//     config, the same way a config-only key stopped being an admin key when
//     it was removed before keys were synced. Keys the first migration seeded
//     and keys added or removed with the user commands since are never
//     removed. Nothing is removed while an initial_admin_keys entry cannot be
//     read or parsed, since that entry may be a key that is still wanted.
//
// Keys are inserted and removed in sorted order, so boots that run at the
// same time on a shared database take row locks in the same order and do not
// deadlock. All instances sharing a database must use the same
// initial_admin_keys: a run with a different list attaches and removes keys to
// match its own list.
func syncAdminKeys(ctx context.Context, tx *db.Tx) error {
	cfg := config.FromContext(ctx)
	if cfg == nil {
		return nil
	}

	logger := log.FromContext(ctx).WithPrefix("migrate")

	keys := cfg.AdminKeys()
	slices.SortFunc(keys, func(a, b gossh.PublicKey) int {
		return strings.Compare(sshutils.MarshalAuthorizedKey(a), sshutils.MarshalAuthorizedKey(b))
	})
	configured := make(map[string]struct{}, len(keys))

	// The admin a configured key already belongs to, so that all configured
	// keys end up on one account.
	var targetID int64
	var haveTarget bool
	var missing []gossh.PublicKey
	for _, k := range keys {
		ak := sshutils.MarshalAuthorizedKey(k)
		configured[ak] = struct{}{}

		o, ok, err := keyOwner(ctx, tx, ak)
		if err != nil {
			return err
		}
		switch {
		case !ok:
			missing = append(missing, k)
		case !o.Admin:
			logger.Warn("initial admin key belongs to a non-admin user; not promoting the user",
				"user", o.Username, "key", gossh.FingerprintSHA256(k))
		case !haveTarget:
			targetID, haveTarget = o.ID, true
		}
	}

	if len(missing) > 0 && !haveTarget {
		id, ok, err := adminUserID(ctx, tx)
		if err != nil {
			return err
		}
		if ok {
			targetID, haveTarget = id, true
		} else {
			logger.Warn("no admin user to attach initial admin keys to, and the username is taken by a non-admin user",
				"user", defaultAdminUsername)
		}
	}

	if haveTarget {
		for _, k := range missing {
			ak := sshutils.MarshalAuthorizedKey(k)
			added, err := insertPublicKey(ctx, tx, targetID, ak)
			if err != nil {
				return fmt.Errorf("add initial admin key: %w", err)
			}
			if !added {
				// A concurrent boot attached it first and recorded it.
				continue
			}
			if err := recordSyncedKey(ctx, tx, ak, targetID); err != nil {
				return err
			}
			logger.Info("initial admin key attached to admin user",
				"user_id", targetID, "key", gossh.FingerprintSHA256(k))
		}
	}

	if invalid := cfg.InvalidAdminKeys(); len(invalid) > 0 {
		logger.Warn("some initial_admin_keys entries could not be read or parsed; not removing synced admin keys until they can",
			"entries", invalid)
		return nil
	}

	return removeUnconfiguredKeys(ctx, tx, logger, configured)
}

type keyOwnerRow struct {
	ID       int64  `db:"id"`
	Username string `db:"username"`
	Admin    bool   `db:"admin"`
}

// keyOwner returns the account that owns ak, if any.
func keyOwner(ctx context.Context, tx *db.Tx, ak string) (keyOwnerRow, bool, error) {
	var o keyOwnerRow
	err := tx.GetContext(ctx, &o, tx.Rebind(`SELECT users.id, users.username, users.admin
		FROM users
		INNER JOIN public_keys ON users.id = public_keys.user_id
		WHERE public_keys.public_key = ?`), ak)
	if errors.Is(err, sql.ErrNoRows) {
		return o, false, nil
	}
	if err != nil {
		return o, false, fmt.Errorf("look up owner of initial admin key: %w", err)
	}
	return o, true, nil
}

// adminUserID returns the id of the admin account new initial admin keys go
// to when no admin owns a configured key yet: the admin named "admin", else
// the admin with the lowest id. It creates the default admin account when
// there is no admin at all. ok is false when there is no admin and the
// default username belongs to a non-admin account, which is never promoted.
func adminUserID(ctx context.Context, tx *db.Tx) (id int64, ok bool, err error) {
	err = tx.GetContext(ctx, &id, tx.Rebind(`SELECT id FROM users WHERE admin = ?
		ORDER BY CASE WHEN username = ? THEN 0 ELSE 1 END, id LIMIT 1`), true, defaultAdminUsername)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("look up admin user: %w", err)
	}

	insert := insertIgnore(tx, `INTO users (username, admin, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)`, "username")
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

// insertPublicKey attaches ak to userID. added is false when the key was
// already there, for example because a concurrent boot inserted it. Any other
// conflict, such as a primary key clash, is returned: on postgres it has
// aborted the transaction.
func insertPublicKey(ctx context.Context, tx *db.Tx, userID int64, ak string) (added bool, err error) {
	query := insertIgnore(tx, `INTO public_keys (user_id, public_key, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)`, "public_key")
	res, err := tx.ExecContext(ctx, query, userID, ak)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("add initial admin key: %w", err)
	}
	return n > 0, nil
}

// recordSyncedKey records that syncAdminKeys attached ak to userID,
// replacing a stale record left by a key that was deleted and attached again.
func recordSyncedKey(ctx context.Context, tx *db.Tx, ak string, userID int64) error {
	name := syncedKeyPrefix + ak
	if _, err := tx.ExecContext(ctx, tx.Rebind(`DELETE FROM settings WHERE "key" = ?`), name); err != nil {
		return fmt.Errorf("record initial admin key: %w", err)
	}
	insert := insertIgnore(tx, `INTO settings ("key", value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)`, `"key"`)
	if _, err := tx.ExecContext(ctx, insert, name, strconv.FormatInt(userID, 10)); err != nil {
		return fmt.Errorf("record initial admin key: %w", err)
	}
	return nil
}

// syncedKeyRow is a settings row recording a key syncAdminKeys attached.
type syncedKeyRow struct {
	Key   string `db:"key"`
	Value string `db:"value"`
}

// removeUnconfiguredKeys removes the keys syncAdminKeys attached that are no
// longer in the config. A key is removed only from the user it was attached
// to; if it has since moved to another account, it is left there.
func removeUnconfiguredKeys(ctx context.Context, tx *db.Tx, logger *log.Logger, configured map[string]struct{}) error {
	var rows []syncedKeyRow
	// "_" is a LIKE wildcard; the prefix is checked again below.
	if err := tx.SelectContext(ctx, &rows, tx.Rebind(`SELECT "key", value FROM settings WHERE "key" LIKE ?`),
		syncedKeyPrefix+"%"); err != nil {
		return fmt.Errorf("list synced initial admin keys: %w", err)
	}
	// Remove in a fixed order, independent of the database's collation, so
	// concurrent boots lock rows in the same order.
	slices.SortFunc(rows, func(a, b syncedKeyRow) int {
		return strings.Compare(a.Key, b.Key)
	})

	for _, r := range rows {
		ak, ok := strings.CutPrefix(r.Key, syncedKeyPrefix)
		if !ok {
			continue
		}
		if _, ok := configured[ak]; ok {
			continue
		}

		if userID, err := strconv.ParseInt(r.Value, 10, 64); err == nil {
			res, err := tx.ExecContext(ctx, tx.Rebind(`DELETE FROM public_keys WHERE public_key = ? AND user_id = ?`), ak, userID)
			if err != nil {
				return fmt.Errorf("remove initial admin key: %w", err)
			}
			if n, err := res.RowsAffected(); err == nil && n > 0 {
				fp := ak
				if pk, _, err := sshutils.ParseAuthorizedKey(ak); err == nil {
					fp = gossh.FingerprintSHA256(pk)
				}
				logger.Warn("initial admin key removed from the config; removed it from the admin user",
					"user_id", userID, "key", fp)
			}
		}

		if _, err := tx.ExecContext(ctx, tx.Rebind(`DELETE FROM settings WHERE "key" = ?`), r.Key); err != nil {
			return fmt.Errorf("forget initial admin key: %w", err)
		}
	}
	return nil
}

// insertIgnore builds an INSERT that does nothing on a unique conflict on
// column, for the transaction's driver. rest starts at "INTO". Naming the
// column keeps an unrelated conflict, such as a primary key clash, an error
// on both sqlite (3.24 and later) and postgres, unlike INSERT OR IGNORE.
func insertIgnore(tx *db.Tx, rest, column string) string {
	switch tx.DriverName() {
	case "sqlite3", "sqlite", "postgres":
		return tx.Rebind("INSERT " + rest + " ON CONFLICT (" + column + ") DO NOTHING")
	default:
		return tx.Rebind("INSERT " + rest)
	}
}
