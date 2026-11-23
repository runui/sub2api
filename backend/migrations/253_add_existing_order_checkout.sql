ALTER TABLE payment_orders
    ADD COLUMN IF NOT EXISTS checkout_key_hash varchar(64),
    ADD COLUMN IF NOT EXISTS checkout_request_hash varchar(64),
    ADD COLUMN IF NOT EXISTS checkout_plan_name varchar(100),
    ADD COLUMN IF NOT EXISTS checkout_plan_currency varchar(3),
    ADD COLUMN IF NOT EXISTS checkout_started_at timestamptz,
    ADD COLUMN IF NOT EXISTS checkout_reconciled_at timestamptz;

CREATE UNIQUE INDEX IF NOT EXISTS paymentorder_user_id_checkout_key_hash
    ON payment_orders (user_id, checkout_key_hash);

CREATE INDEX IF NOT EXISTS paymentorder_checkout_started_at
    ON payment_orders (checkout_started_at) WHERE checkout_started_at IS NOT NULL;
