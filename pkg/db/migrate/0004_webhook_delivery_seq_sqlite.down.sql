-- Rebuild the table in its original shape: id is the primary key again and
-- seq is gone. Rows keep their seq order as rowid order.
CREATE TABLE webhook_deliveries_old (
  id TEXT PRIMARY KEY,
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

INSERT INTO webhook_deliveries_old (id, webhook_id, event, request_url, request_method, request_error, request_headers, request_body, response_status, response_headers, response_body, created_at)
SELECT id, webhook_id, event, request_url, request_method, request_error, request_headers, request_body, response_status, response_headers, response_body, created_at
FROM webhook_deliveries
ORDER BY seq;

DROP TABLE webhook_deliveries;
ALTER TABLE webhook_deliveries_old RENAME TO webhook_deliveries;
