DROP TRIGGER payments_status_transition ON payments;
DROP FUNCTION enforce_payment_status_transition();
DROP TABLE simulated_provider_operations;
ALTER TABLE payments DROP CONSTRAINT payments_operation_required;
ALTER TABLE payments DROP CONSTRAINT payments_claim_consistency;
ALTER TABLE payments DROP CONSTRAINT payments_status_timestamps;
ALTER TABLE payments
    DROP COLUMN provider_operation_id,
    DROP COLUMN processing_scenario,
    DROP COLUMN processing_token,
    DROP COLUMN processing_lease_until,
    DROP COLUMN processing_started_at,
    DROP COLUMN completed_at,
    DROP COLUMN failed_at,
    DROP COLUMN reconciliation_required_at,
    DROP COLUMN last_processing_error;
ALTER TABLE payments DROP CONSTRAINT payments_status_check;
ALTER TABLE payments ADD CONSTRAINT payments_status_check CHECK (status = 'CREATED');
