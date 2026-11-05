-- Adding the column without a default is a catalog-only change; the backfill
-- below then numbers existing rows deterministically before new rows take
-- values from the sequence.
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

CREATE UNIQUE INDEX IF NOT EXISTS webhook_deliveries_seq_idx ON webhook_deliveries (seq);
CREATE INDEX IF NOT EXISTS webhook_deliveries_webhook_id_seq_idx ON webhook_deliveries (webhook_id, seq);
