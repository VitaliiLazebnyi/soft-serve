DROP TRIGGER IF EXISTS webhook_deliveries_set_seq;
DROP INDEX IF EXISTS webhook_deliveries_webhook_id_seq_idx;
DROP INDEX IF EXISTS webhook_deliveries_seq_idx;
ALTER TABLE webhook_deliveries DROP COLUMN seq;
