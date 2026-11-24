ALTER TABLE composite_model_routes DROP CONSTRAINT IF EXISTS composite_model_routes_endpoint_check;
ALTER TABLE composite_model_routes ADD CONSTRAINT composite_model_routes_endpoint_check
    CHECK (endpoint IN ('any', 'messages', 'count_tokens', 'responses', 'chat_completions', 'embeddings', 'images', 'videos', 'gemini'));
ALTER TABLE composite_model_routes DROP CONSTRAINT IF EXISTS composite_model_routes_target_platform_check;
ALTER TABLE composite_model_routes ADD CONSTRAINT composite_model_routes_target_platform_check
    CHECK ((endpoint = 'videos' AND target_platform IN ('grok', 'fal')) OR
           (endpoint <> 'videos' AND target_platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'grok')));

UPDATE channels
SET features_config = features_config - 'fal_video_routes'
WHERE jsonb_typeof(features_config) = 'object' AND features_config ? 'fal_video_routes';

-- Stop old gateway processes before upgrading. Only refund outstanding fal
-- reservations: captured charges and holds for other features are not touched.
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
        RAISE EXCEPTION 'Cannot remove legacy fal video tasks: invalid reservation amount';
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
            RAISE EXCEPTION 'Cannot remove legacy fal video tasks: missing hold for user %', hold.user_id;
        END IF;
    END LOOP;
END $$;

DELETE FROM fal_video_tasks;
