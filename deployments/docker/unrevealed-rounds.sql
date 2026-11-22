-- A development database older than migration 0010 has no sealed secrets.
SELECT to_regclass('orvex.round_secret_reveals') IS NOT NULL AS has_reveals \gset
\if :has_reveals
SELECT count(*) FROM orvex.round_secrets s
WHERE s.secret_ciphertext IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM orvex.round_secret_reveals v WHERE v.round_id = s.round_id);
\else
SELECT 0;
\endif
