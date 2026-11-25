-- This is discovery evidence, not an execution allowlist or a group price table.
CREATE TABLE IF NOT EXISTS account_model_catalogs (
    account_id bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    source text NOT NULL CHECK (source <> ''),
    started_at timestamptz NOT NULL,
    catalog jsonb NOT NULL,
    PRIMARY KEY (account_id, source)
);
