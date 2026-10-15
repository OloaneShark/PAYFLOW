-- A request is committed before any attempt to contact Temporal. The worker
-- scans undispatched rows after restarts and Temporal outages.
CREATE TABLE payment_workflow_dispatch (
    payment_id UUID PRIMARY KEY REFERENCES payments(id),
    scenario TEXT NOT NULL CHECK (scenario IN (
        'APPROVE', 'DECLINE', 'TEMPORARY_FAILURE', 'NETWORK_TIMEOUT', 'LOST_APPROVAL_RESPONSE'
    )),
    workflow_id TEXT NOT NULL UNIQUE,
    requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    dispatched_at TIMESTAMPTZ,
    last_dispatch_error TEXT
);

CREATE INDEX payment_workflow_dispatch_pending_idx
    ON payment_workflow_dispatch (requested_at) WHERE dispatched_at IS NULL;
