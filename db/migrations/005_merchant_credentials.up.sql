-- Credentials are scoped to one merchant. Only SHA-256 digests of 256-bit
-- random API keys are stored; the plaintext key is returned once at issuance.
CREATE TABLE merchant_api_keys (
    id UUID PRIMARY KEY,
    merchant_id TEXT NOT NULL CHECK (merchant_id <> ''),
    key_hash BYTEA NOT NULL UNIQUE CHECK (octet_length(key_hash) = 32),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ
);
CREATE INDEX merchant_api_keys_active_merchant_idx
    ON merchant_api_keys (merchant_id) WHERE revoked_at IS NULL;

-- The API never queries unscoped tenant data. This index also makes the
-- database-side ownership lookup cheap without changing existing payments.
CREATE INDEX payments_merchant_id_id_idx ON payments (merchant_id, id);
CREATE INDEX payments_expired_claim_idx ON payments (processing_lease_until)
    WHERE status = 'PROCESSING';
CREATE INDEX payments_unresolved_idx ON payments (updated_at)
    WHERE status = 'RECONCILIATION_REQUIRED';

ALTER TABLE payments
    ADD COLUMN recovery_attempts INTEGER NOT NULL DEFAULT 0 CHECK (recovery_attempts >= 0),
    ADD COLUMN recovery_next_attempt_at TIMESTAMPTZ;
