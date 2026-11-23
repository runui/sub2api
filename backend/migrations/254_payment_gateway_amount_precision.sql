-- Keep gateway amounts exact for currencies with three decimal places.
ALTER TABLE payment_orders
    ALTER COLUMN pay_amount TYPE DECIMAL(21,3),
    ALTER COLUMN refund_amount TYPE DECIMAL(21,3);
