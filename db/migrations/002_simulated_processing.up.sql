ALTER TABLE payments DROP CONSTRAINT payments_status_check;
ALTER TABLE payments ADD CONSTRAINT payments_status_check CHECK (
    status IN ('CREATED', 'PROCESSING', 'COMPLETED', 'FAILED', 'RECONCILIATION_REQUIRED')
);

ALTER TABLE payments
    ADD COLUMN provider_operation_id UUID UNIQUE,
    ADD COLUMN processing_scenario TEXT CHECK (processing_scenario IN (
        'APPROVE', 'DECLINE', 'TEMPORARY_FAILURE', 'NETWORK_TIMEOUT', 'LOST_APPROVAL_RESPONSE'
    )),
    ADD COLUMN processing_token UUID,
    ADD COLUMN processing_lease_until TIMESTAMPTZ,
    ADD COLUMN processing_started_at TIMESTAMPTZ,
    ADD COLUMN completed_at TIMESTAMPTZ,
    ADD COLUMN failed_at TIMESTAMPTZ,
    ADD COLUMN reconciliation_required_at TIMESTAMPTZ,
    ADD COLUMN last_processing_error TEXT,
    ADD CONSTRAINT payments_operation_required CHECK (
        (status = 'CREATED' AND provider_operation_id IS NULL AND processing_scenario IS NULL)
        OR (status <> 'CREATED' AND provider_operation_id IS NOT NULL AND processing_scenario IS NOT NULL)
    ),
    ADD CONSTRAINT payments_claim_consistency CHECK (
        (status = 'PROCESSING' AND processing_token IS NOT NULL AND processing_lease_until IS NOT NULL)
        OR (status <> 'PROCESSING' AND processing_token IS NULL AND processing_lease_until IS NULL)
    ),
    ADD CONSTRAINT payments_status_timestamps CHECK (
        (status = 'CREATED' OR processing_started_at IS NOT NULL)
        AND (status <> 'COMPLETED' OR completed_at IS NOT NULL)
        AND (status <> 'FAILED' OR failed_at IS NOT NULL)
        AND (status <> 'RECONCILIATION_REQUIRED' OR reconciliation_required_at IS NOT NULL)
    );

CREATE TABLE simulated_provider_operations (
    operation_id UUID PRIMARY KEY,
    payment_id UUID NOT NULL UNIQUE REFERENCES payments(id),
    scenario TEXT NOT NULL CHECK (scenario IN (
        'APPROVE', 'DECLINE', 'TEMPORARY_FAILURE', 'NETWORK_TIMEOUT', 'LOST_APPROVAL_RESPONSE'
    )),
    outcome TEXT NOT NULL CHECK (outcome IN ('PENDING', 'APPROVED', 'DECLINED')),
    attempts INTEGER NOT NULL DEFAULT 1 CHECK (attempts > 0),
    logical_charge_id UUID UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT provider_charge_matches_outcome CHECK (
        (outcome = 'APPROVED' AND logical_charge_id = operation_id)
        OR (outcome <> 'APPROVED' AND logical_charge_id IS NULL)
    )
);

CREATE FUNCTION enforce_payment_status_transition() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status <> 'CREATED' AND (
        NEW.provider_operation_id IS DISTINCT FROM OLD.provider_operation_id
        OR NEW.processing_scenario IS DISTINCT FROM OLD.processing_scenario
    ) THEN
        RAISE EXCEPTION 'provider operation and scenario cannot change after processing starts'
            USING ERRCODE = '23514';
    END IF;
    IF NEW.status = OLD.status THEN
        RETURN NEW;
    END IF;
    IF NEW.status = 'COMPLETED' AND NOT EXISTS (
        SELECT 1 FROM simulated_provider_operations
        WHERE operation_id = NEW.provider_operation_id AND payment_id = NEW.id AND outcome = 'APPROVED'
    ) THEN
        RAISE EXCEPTION 'COMPLETED requires a confirmed provider approval'
            USING ERRCODE = '23514';
    END IF;
    IF NEW.status = 'FAILED' AND NOT EXISTS (
        SELECT 1 FROM simulated_provider_operations
        WHERE operation_id = NEW.provider_operation_id AND payment_id = NEW.id AND outcome = 'DECLINED'
    ) THEN
        RAISE EXCEPTION 'FAILED requires a confirmed provider decline'
            USING ERRCODE = '23514';
    END IF;
    IF (OLD.status = 'CREATED' AND NEW.status = 'PROCESSING')
       OR (OLD.status = 'PROCESSING' AND NEW.status IN ('COMPLETED', 'FAILED', 'RECONCILIATION_REQUIRED'))
       OR (OLD.status = 'RECONCILIATION_REQUIRED' AND NEW.status = 'PROCESSING') THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'invalid payment status transition: % -> %', OLD.status, NEW.status
        USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER payments_status_transition
    BEFORE UPDATE ON payments
    FOR EACH ROW EXECUTE FUNCTION enforce_payment_status_transition();
