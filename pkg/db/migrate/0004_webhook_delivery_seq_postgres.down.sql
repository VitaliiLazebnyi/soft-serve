DROP INDEX IF EXISTS webhook_deliveries_webhook_id_seq_idx;
DROP INDEX IF EXISTS webhook_deliveries_seq_idx;
-- Dropping the column also drops the sequence it owns.
ALTER TABLE webhook_deliveries DROP COLUMN IF EXISTS seq;
