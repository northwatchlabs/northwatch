UPDATE components
SET updated_at = updated_at * 1000
WHERE updated_at > 0
  AND updated_at < 1000000000000;
