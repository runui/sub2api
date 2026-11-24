CREATE TABLE IF NOT EXISTS fal_video_tasks (
    id                  VARCHAR(64) PRIMARY KEY,
    user_id             BIGINT NOT NULL,
    api_key_id          BIGINT NOT NULL,
    group_id            BIGINT NOT NULL,
    subscription_id     BIGINT NOT NULL DEFAULT 0,
    account_id          BIGINT NOT NULL,
    model               VARCHAR(255) NOT NULL,
    endpoint            VARCHAR(255) NOT NULL,
    resolution          VARCHAR(32) NOT NULL,
    duration            INTEGER NOT NULL CHECK (duration >= 0),
    unit_price_usd      DOUBLE PRECISION NOT NULL CHECK (unit_price_usd >= 0 AND unit_price_usd < 'Infinity'::DOUBLE PRECISION),
    idempotency_key     VARCHAR(255) NOT NULL DEFAULT '',
    fingerprint         VARCHAR(128) NOT NULL,
    state               VARCHAR(16) NOT NULL DEFAULT 'submitting'
                        CHECK (state IN ('submitting', 'queued', 'running', 'pending_success', 'pending_failure', 'succeeded', 'failed', 'unknown')),
    billing_state       TEXT NOT NULL DEFAULT 'unreserved'
                        CHECK (billing_state IN ('unreserved', 'reserved', 'captured', 'released')),
    reference           JSON,
    result_url          TEXT NOT NULL DEFAULT '',
    actual_duration     DOUBLE PRECISION NOT NULL DEFAULT 0 CHECK (actual_duration >= 0 AND actual_duration < 'Infinity'::DOUBLE PRECISION),
    width               INTEGER NOT NULL DEFAULT 0 CHECK (width >= 0),
    height              INTEGER NOT NULL DEFAULT 0 CHECK (height >= 0),
    error_code          VARCHAR(128) NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    next_poll_at        TIMESTAMPTZ,
    lease_token         UUID,
    lease_until         TIMESTAMPTZ,
    CHECK ((next_poll_at IS NOT NULL) = (state IN ('queued', 'running', 'pending_success', 'pending_failure'))),
    CHECK ((lease_token IS NULL) = (lease_until IS NULL)),
    CHECK (lease_token IS NULL OR next_poll_at IS NOT NULL)
);

CREATE UNIQUE INDEX IF NOT EXISTS fal_video_tasks_idempotency_idx
    ON fal_video_tasks (user_id, api_key_id, group_id, idempotency_key)
    WHERE idempotency_key <> '';

CREATE INDEX IF NOT EXISTS fal_video_tasks_due_idx
    ON fal_video_tasks (next_poll_at, id)
    WHERE state IN ('queued', 'running', 'pending_success', 'pending_failure');

CREATE INDEX IF NOT EXISTS fal_video_tasks_submitting_idx
    ON fal_video_tasks (updated_at)
    WHERE state = 'submitting';

CREATE INDEX IF NOT EXISTS fal_video_tasks_reserved_key_idx
    ON fal_video_tasks (api_key_id)
    WHERE billing_state = 'reserved';
