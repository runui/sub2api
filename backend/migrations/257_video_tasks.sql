-- Stop old workers before this migration. Discard obsolete tasks, but return
-- their outstanding holds in the same transaction before deleting any rows.
LOCK TABLE fal_video_tasks IN ACCESS EXCLUSIVE MODE;

-- Match the billing code's shortest-decimal conversion before rounding each task.
SET LOCAL extra_float_digits = 3;

DO $$
DECLARE
    hold RECORD;
BEGIN
    IF EXISTS (
        SELECT 1 FROM fal_video_tasks
        WHERE billing_state = 'reserved' AND round((unit_price_usd * duration)::text::numeric, 8) <= 0
    ) THEN
        RAISE EXCEPTION 'Cannot reset old video tasks: invalid reservation amount';
    END IF;

    FOR hold IN
        SELECT user_id, SUM(round((unit_price_usd * duration)::text::numeric, 8)) AS amount
        FROM fal_video_tasks WHERE billing_state = 'reserved'
        GROUP BY user_id ORDER BY user_id
    LOOP
        UPDATE users SET balance = balance + hold.amount,
            frozen_balance = frozen_balance - hold.amount, updated_at = NOW()
        WHERE id = hold.user_id AND frozen_balance >= hold.amount;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'Cannot reset old video tasks: missing hold for user %', hold.user_id;
        END IF;
    END LOOP;
END $$;

DELETE FROM fal_video_tasks;

ALTER TABLE fal_video_tasks RENAME TO video_tasks;
ALTER INDEX fal_video_tasks_pkey RENAME TO video_tasks_pkey;
ALTER INDEX fal_video_tasks_idempotency_idx RENAME TO video_tasks_idempotency_idx;
ALTER INDEX fal_video_tasks_reserved_key_idx RENAME TO video_tasks_reserved_key_idx;

ALTER TABLE video_tasks
    ADD COLUMN provider text NOT NULL CHECK (provider <> ''),
    ADD COLUMN profile_version text NOT NULL CHECK (profile_version <> ''),
    ADD COLUMN result_path text NOT NULL CHECK (result_path <> ''),
    ADD COLUMN quoted_amount_usd numeric(24,8) NOT NULL
        CHECK (quoted_amount_usd > 0 AND quoted_amount_usd < 'Infinity'::numeric),
    DROP COLUMN unit_price_usd;

DROP INDEX fal_video_tasks_due_idx;
CREATE INDEX video_tasks_due_idx ON video_tasks (provider, next_poll_at, id)
    WHERE state IN ('queued', 'running', 'pending_success', 'pending_failure');
DROP INDEX fal_video_tasks_submitting_idx;
CREATE INDEX video_tasks_submitting_idx ON video_tasks (provider, updated_at) WHERE state = 'submitting';

-- 256 restored a provider whitelist. Keep provider membership in the
-- application registry (242); the endpoint CHECK still constrains route kinds.
ALTER TABLE composite_model_routes DROP CONSTRAINT IF EXISTS composite_model_routes_target_platform_check;
