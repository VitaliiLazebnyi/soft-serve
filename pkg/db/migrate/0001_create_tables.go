package migrate

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
)

const (
	createTablesName    = "create tables"
	createTablesVersion = 1
)

var createTables = Migration{
	Version: createTablesVersion,
	Name:    createTablesName,
	Migrate: func(ctx context.Context, tx *db.Tx) error {
		cfg := config.FromContext(ctx)

		insert := "INSERT "

		// Alter old tables (if exist)
		// This is to support prior versions of Soft Serve v0.6
		switch tx.DriverName() {
		case "sqlite3", "sqlite":
			insert += "OR IGNORE "

			if ok, err := tableExists(tx, "user"); err != nil {
				return err
			} else if ok {
				if _, err := tx.ExecContext(ctx, "ALTER TABLE user RENAME TO user_old"); err != nil {
					return err
				}
			}

			if ok, err := tableExists(tx, "public_key"); err != nil {
				return err
			} else if ok {
				if _, err := tx.ExecContext(ctx, "ALTER TABLE public_key RENAME TO public_key_old"); err != nil {
					return err
				}
			}

			if ok, err := tableExists(tx, "collab"); err != nil {
				return err
			} else if ok {
				if _, err := tx.ExecContext(ctx, "ALTER TABLE collab RENAME TO collab_old"); err != nil {
					return err
				}
			}

			if ok, err := tableExists(tx, "repo"); err != nil {
				return err
			} else if ok {
				if _, err := tx.ExecContext(ctx, "ALTER TABLE repo RENAME TO repo_old"); err != nil {
					return err
				}
			}
		}

		if err := migrateUp(ctx, tx, createTablesVersion, createTablesName); err != nil {
			return err
		}

		switch tx.DriverName() {
		case "sqlite3", "sqlite":

			if _, err := tx.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
				return err
			}

			if ok, err := tableExists(tx, "user_old"); err != nil {
				return err
			} else if ok {
				sqlm := `
				INSERT INTO users (id, username, admin, updated_at)
					SELECT id, username, admin, updated_at FROM user_old;
				`
				if _, err := tx.ExecContext(ctx, sqlm); err != nil {
					return err
				}
			}

			if ok, err := tableExists(tx, "public_key_old"); err != nil {
				return err
			} else if ok {
				// Check duplicate keys
				pks := []struct {
					ID        string `db:"id"`
					PublicKey string `db:"public_key"`
				}{}
				if err := tx.SelectContext(ctx, &pks, "SELECT id, public_key FROM public_key_old"); err != nil {
					return err
				}

				pkss := map[string]struct{}{}
				for _, pk := range pks {
					if _, ok := pkss[pk.PublicKey]; ok {
						return fmt.Errorf("duplicate public key: %q, please remove the duplicate key and try again", pk.PublicKey)
					}
					pkss[pk.PublicKey] = struct{}{}
				}

				sqlm := `
				INSERT INTO public_keys (id, user_id, public_key, created_at, updated_at)
					SELECT id, user_id, public_key, created_at, updated_at FROM public_key_old;
				`
				if _, err := tx.ExecContext(ctx, sqlm); err != nil {
					return err
				}
			}

			if ok, err := tableExists(tx, "repo_old"); err != nil {
				return err
			} else if ok {
				sqlm := `
				INSERT INTO repos (id, name, project_name, description, private,mirror, hidden, created_at, updated_at, user_id)
					SELECT id, name, project_name, description, private, mirror, hidden, created_at, updated_at, (
						SELECT id FROM users WHERE admin = true ORDER BY id LIMIT 1
				) FROM repo_old;
				`
				if _, err := tx.ExecContext(ctx, sqlm); err != nil {
					return err
				}
			}

			if ok, err := tableExists(tx, "collab_old"); err != nil {
				return err
			} else if ok {
				sqlm := `
				INSERT INTO collabs (id, user_id, repo_id, access_level, created_at, updated_at)
					SELECT id, user_id, repo_id, ` + strconv.Itoa(int(access.ReadWriteAccess)) + `, created_at, updated_at FROM collab_old;
				`
				if _, err := tx.ExecContext(ctx, sqlm); err != nil {
					return err
				}
			}

			if _, err := tx.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
				return err
			}
		}

		// Insert default user
		insertUser := tx.Rebind(insert + "INTO users (username, admin, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)")
		if _, err := tx.ExecContext(ctx, insertUser, "admin", true); err != nil {
			return err
		}

		// Seed the keys to the admin named "admin", not to whatever account
		// has id 1: with imported legacy users that may be anyone. When there
		// is no such admin, the keys are left out (with a warning) and stay
		// admin keys through the config.
		adminKeys := cfg.AdminKeys()
		var adminID int64
		if len(adminKeys) > 0 {
			var ok bool
			var err error
			adminID, ok, err = adminUserID(ctx, tx, log.FromContext(ctx).WithPrefix("migrate"))
			if err != nil {
				return err
			}
			if !ok {
				adminKeys = nil
			}
		}
		for _, k := range adminKeys {
			query := insert + "INTO public_keys (user_id, public_key, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)"
			if tx.DriverName() == "postgres" {
				query += " ON CONFLICT DO NOTHING"
			}

			query = tx.Rebind(query)
			ak := sshutils.MarshalAuthorizedKey(k)
			if _, err := tx.ExecContext(ctx, query, adminID, ak); err != nil {
				if errors.Is(db.WrapError(err), db.ErrDuplicateKey) {
					continue
				}
				return err
			}
		}

		// Insert default settings
		insertSettings := insert + "INTO settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)"
		insertSettings = tx.Rebind(insertSettings)
		settings := []struct {
			Key   string
			Value string
		}{
			{"allow_keyless", "true"},
			{"anon_access", access.ReadOnlyAccess.String()},
			{"init", "true"},
		}

		for _, s := range settings {
			if _, err := tx.ExecContext(ctx, insertSettings, s.Key, s.Value); err != nil {
				return fmt.Errorf("inserting default settings %q: %w", s.Key, err)
			}
		}

		return nil
	},
	Rollback: func(ctx context.Context, tx *db.Tx) error {
		return migrateDown(ctx, tx, createTablesVersion, createTablesName)
	},
}
