-- 0003_token_prefix.sql (Postgres) — identifying prefix for per-cluster
-- ingest tokens, so `tokens list` can show which deployed secret a row is
-- (store.TokenPrefix: first 8 plaintext chars, '' for short tokens).
-- Tokens minted before this migration keep '' — their plaintext is gone.
-- Identical to migrations/0003_token_prefix.sql.

ALTER TABLE tokens ADD COLUMN token_prefix TEXT NOT NULL DEFAULT '';
