CREATE TABLE payments (
    id UUID PRIMARY KEY,
    merchant_id TEXT NOT NULL CHECK (merchant_id <> ''),
    idempotency_key TEXT NOT NULL CHECK (idempotency_key <> ''),
    request_fingerprint BYTEA NOT NULL CHECK (octet_length(request_fingerprint) = 32),
    amount BIGINT NOT NULL CHECK (amount > 0),
    currency TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'CREATED' CHECK (status = 'CREATED'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT payments_merchant_idempotency_key_unique UNIQUE (merchant_id, idempotency_key)
);
