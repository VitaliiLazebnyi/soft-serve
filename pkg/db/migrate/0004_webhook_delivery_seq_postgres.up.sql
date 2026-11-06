-- Adding the column without a default is a catalog-only change; the backfill
-- below then numbers existing rows before new rows take values from the
-- sequence.
--
-- Existing rows are numbered by created_at, then by id. Postgres has no
-- insertion order to fall back on, and ids are random UUIDs, so the order of
-- old rows recorded within the same second is arbitrary (but fixed from then
-- on). The backfill writes a new version of every row, and SET NOT NULL and
-- the index builds scan the table, all in the migration transaction while
-- holding an exclusive lock. Deliveries are never pruned, so this can take a
-- while on a server that has recorded many of them.
--
-- New rows take seq from the sequence when they are inserted, not when they
-- commit, and a rolled-back insert leaves its number unused. A listing taken
-- while inserts are in flight can therefore show a gap that fills in later.
-- Numbers are never reused.
ALTER TABLE webhook_deliveries ADD COLUMN seq BIGINT;

UPDATE webhook_deliveries AS d
SET seq = o.rn
FROM (
  SELECT id, ROW_NUMBER() OVER (ORDER BY created_at, id) AS rn
  FROM webhook_deliveries
) AS o
WHERE d.id = o.id;

CREATE SEQUENCE IF NOT EXISTS webhook_deliveries_seq_seq OWNED BY webhook_deliveries.seq;
SELECT setval('webhook_deliveries_seq_seq', COALESCE((SELECT MAX(seq) FROM webhook_deliveries), 0) + 1, false);
ALTER TABLE webhook_deliveries ALTER COLUMN seq SET DEFAULT nextval('webhook_deliveries_seq_seq');
ALTER TABLE webhook_deliveries ALTER COLUMN seq SET NOT NULL;

-- The sequence only guarantees unique values for rows that take the default;
-- the unique index also rejects an explicit seq that collides.
CREATE UNIQUE INDEX IF NOT EXISTS webhook_deliveries_seq_idx ON webhook_deliveries (seq);
CREATE INDEX IF NOT EXISTS webhook_deliveries_webhook_id_seq_idx ON webhook_deliveries (webhook_id, seq);
