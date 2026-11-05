-- SQLite cannot add an AUTOINCREMENT column to an existing table, so seq is a
-- plain column filled by a trigger. SQLite serializes writers, so MAX(seq) + 1
-- cannot race.
ALTER TABLE webhook_deliveries ADD COLUMN seq INTEGER;

UPDATE webhook_deliveries
SET seq = o.rn
FROM (
  SELECT rowid AS rid, ROW_NUMBER() OVER (ORDER BY created_at, rowid) AS rn
  FROM webhook_deliveries
) AS o
WHERE webhook_deliveries.rowid = o.rid;

CREATE UNIQUE INDEX IF NOT EXISTS webhook_deliveries_seq_idx ON webhook_deliveries (seq);
CREATE INDEX IF NOT EXISTS webhook_deliveries_webhook_id_seq_idx ON webhook_deliveries (webhook_id, seq);

CREATE TRIGGER IF NOT EXISTS webhook_deliveries_set_seq
AFTER INSERT ON webhook_deliveries
FOR EACH ROW WHEN NEW.seq IS NULL
BEGIN
  UPDATE webhook_deliveries
  SET seq = (SELECT COALESCE(MAX(seq), 0) + 1 FROM webhook_deliveries)
  WHERE rowid = NEW.rowid;
END;
