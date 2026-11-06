-- Keep the 100 most recent deliveries of each webhook, newest by seq.
-- 100 is maxDeliveries in pkg/webhook/webhook.go, which prunes on every
-- recorded delivery from now on. This migration is a one-time snapshot of
-- that value: if maxDeliveries changes, leave this file as it is.
DELETE FROM webhook_deliveries
WHERE seq IN (
  SELECT seq FROM (
    SELECT seq, ROW_NUMBER() OVER (PARTITION BY webhook_id ORDER BY seq DESC) AS rn
    FROM webhook_deliveries
  ) AS ranked
  WHERE rn > 100
);
