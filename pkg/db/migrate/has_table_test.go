package migrate

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/internal/test"
	"github.com/jmoiron/sqlx"
)

// hasTable looks for a table in the schema that unqualified statements use.
// On Postgres that is the first schema on search_path, which need not be
// "public". The test runs on Postgres when SOFT_SERVE_DB_DRIVER is
// "postgres".
func TestHasTable(t *testing.T) {
	ctx := context.TODO()
	dbx, err := test.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}

	if err := dbx.TransactionContext(ctx, func(tx *db.Tx) error {
		if dbx.DriverName() == "postgres" {
			for _, q := range []string{
				`CREATE TABLE public.only_in_public (id INTEGER)`,
				`CREATE SCHEMA app`,
				`SET LOCAL search_path TO app`,
			} {
				if _, err := tx.ExecContext(ctx, q); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}
			if hasTable(tx, "only_in_public") {
				t.Errorf("hasTable(only_in_public) = true with search_path=app, want false")
			}
		}

		if hasTable(tx, "widgets") {
			t.Errorf("hasTable(widgets) before CREATE = true, want false")
		}
		if _, err := tx.ExecContext(ctx, `CREATE TABLE widgets (id INTEGER)`); err != nil {
			t.Fatal(err)
		}
		if !hasTable(tx, "widgets") {
			t.Errorf("hasTable(widgets) after CREATE = false, want true")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// No MySQL driver is linked in, so check the query hasTable sends with a
// driver that records it. MySQL keeps tables in databases, not in a "public"
// schema, so the lookup must be limited to the current database.
func TestHasTableMySQLQuery(t *testing.T) {
	conn := &recordingConn{}
	dbx := sqlx.NewDb(sql.OpenDB(recordingConnector{conn}), "mysql")
	t.Cleanup(func() { dbx.Close() })

	txx, err := dbx.Beginx()
	if err != nil {
		t.Fatal(err)
	}
	defer txx.Rollback() //nolint:errcheck

	if hasTable(&db.Tx{Tx: txx}, "widgets") {
		t.Errorf("hasTable(widgets) = true with no rows, want false")
	}

	want := "SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?"
	if len(conn.queries) != 1 || conn.queries[0] != want {
		t.Errorf("queries = %q, want [%q]", conn.queries, want)
	}
	if len(conn.args) != 1 || conn.args[0] != "widgets" {
		t.Errorf("args = %v, want [widgets]", conn.args)
	}
}

type recordingConnector struct{ conn *recordingConn }

func (c recordingConnector) Connect(context.Context) (driver.Conn, error) { return c.conn, nil }
func (c recordingConnector) Driver() driver.Driver                        { return nil }

// recordingConn records each query and returns no rows.
type recordingConn struct {
	queries []string
	args    []driver.Value
}

func (c *recordingConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}
func (c *recordingConn) Close() error              { return nil }
func (c *recordingConn) Begin() (driver.Tx, error) { return recordingTx{}, nil }

func (c *recordingConn) Query(query string, args []driver.Value) (driver.Rows, error) {
	c.queries = append(c.queries, query)
	c.args = append(c.args, args...)
	return emptyRows{}, nil
}

type recordingTx struct{}

func (recordingTx) Commit() error   { return nil }
func (recordingTx) Rollback() error { return nil }

type emptyRows struct{}

func (emptyRows) Columns() []string         { return []string{"table_name"} }
func (emptyRows) Close() error              { return nil }
func (emptyRows) Next([]driver.Value) error { return io.EOF }
