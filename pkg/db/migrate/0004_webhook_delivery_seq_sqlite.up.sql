-- SQLite cannot add an AUTOINCREMENT column to an existing table, so the
-- table is rebuilt with seq as its INTEGER PRIMARY KEY AUTOINCREMENT. seq is
-- then the rowid: inserts that leave it out get the next value with no extra
-- write, and AUTOINCREMENT keeps a high-water mark in sqlite_sequence, so a
-- number is never reused, even after the newest delivery is deleted. This
-- matches the Postgres sequence. id keeps its uniqueness as a UNIQUE
-- constraint; like the old TEXT PRIMARY KEY it does not add NOT NULL.
--
-- Existing rows are numbered by created_at, then by insertion order (rowid).
-- The copy rewrites the whole table once, bodies included, in the migration
-- transaction. Deliveries are never pruned, so this can take a while on a
-- server that has recorded many of them. Both copies exist until the commit,
-- so the database file grows by about the size of the deliveries table and
-- needs that much free disk; without it the migration fails and rolls back.
-- The old table's pages stay in the file as free pages afterwards (deleting
-- deliveries first does not shrink the file either). VACUUM cannot run in a
-- transaction, so the migration logs a hint to run it once the server is
-- stopped, e.g. `sqlite3 soft-serve.db VACUUM`.
CREATE TABLE webhook_deliveries_new (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  id TEXT UNIQUE,
  webhook_id INTEGER NOT NULL,
  event INTEGER NOT NULL,
  request_url TEXT NOT NULL,
  request_method TEXT NOT NULL,
  request_error TEXT,
  request_headers TEXT NOT NULL,
  request_body TEXT NOT NULL,
  response_status INTEGER NOT NULL,
  response_headers TEXT NOT NULL,
  response_body TEXT NOT NULL,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  CONSTRAINT webhook_id_fk
  FOREIGN KEY(webhook_id) REFERENCES webhooks(id)
  ON DELETE CASCADE
  ON UPDATE CASCADE
);

INSERT INTO webhook_deliveries_new (seq, id, webhook_id, event, request_url, request_method, request_error, request_headers, request_body, response_status, response_headers, response_body, created_at)
SELECT ROW_NUMBER() OVER (ORDER BY created_at, rowid), id, webhook_id, event, request_url, request_method, request_error, request_headers, request_body, response_status, response_headers, response_body, created_at
FROM webhook_deliveries;

DROP TABLE webhook_deliveries;
ALTER TABLE webhook_deliveries_new RENAME TO webhook_deliveries;

CREATE INDEX IF NOT EXISTS webhook_deliveries_webhook_id_seq_idx ON webhook_deliveries (webhook_id, seq);
