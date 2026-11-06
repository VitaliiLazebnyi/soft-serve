-- Keep the 100 most recent deliveries of each webhook, newest by seq.
DELETE FROM webhook_deliveries
WHERE seq IN (
  SELECT seq FROM (
    SELECT seq, ROW_NUMBER() OVER (PARTITION BY webhook_id ORDER BY seq DESC) AS rn
    FROM webhook_deliveries
  ) AS ranked
  WHERE rn > 100
);
