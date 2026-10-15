CREATE TABLE ledger_accounts (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('PROVIDER_RECEIVABLE', 'MERCHANT_PAYABLE')),
    merchant_id TEXT,
    currency TEXT NOT NULL CHECK (currency <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ledger_account_owner CHECK (
        (kind = 'PROVIDER_RECEIVABLE' AND merchant_id IS NULL)
        OR (kind = 'MERCHANT_PAYABLE' AND merchant_id IS NOT NULL AND merchant_id <> '')
    )
);
CREATE UNIQUE INDEX ledger_provider_account_unique ON ledger_accounts (currency)
    WHERE kind = 'PROVIDER_RECEIVABLE';
CREATE UNIQUE INDEX ledger_merchant_account_unique ON ledger_accounts (merchant_id, currency)
    WHERE kind = 'MERCHANT_PAYABLE';

CREATE TABLE ledger_journals (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    payment_id UUID NOT NULL REFERENCES payments(id),
    purpose TEXT NOT NULL CHECK (purpose = 'PAYMENT_APPROVAL'),
    posting_reference TEXT NOT NULL UNIQUE,
    provider_operation_id UUID NOT NULL REFERENCES simulated_provider_operations(operation_id),
    merchant_id TEXT NOT NULL,
    currency TEXT NOT NULL CHECK (currency <> ''),
    amount BIGINT NOT NULL CHECK (amount > 0),
    posted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    posting_txid XID8 NOT NULL DEFAULT pg_current_xact_id(),
    CONSTRAINT ledger_one_purpose_per_payment UNIQUE (payment_id, purpose)
);

CREATE TABLE ledger_entries (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    journal_id BIGINT NOT NULL REFERENCES ledger_journals(id),
    account_id BIGINT NOT NULL REFERENCES ledger_accounts(id),
    side TEXT NOT NULL CHECK (side IN ('DEBIT', 'CREDIT')),
    currency TEXT NOT NULL CHECK (currency <> ''),
    amount BIGINT NOT NULL CHECK (amount > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ledger_one_entry_per_side UNIQUE (journal_id, side)
);
CREATE INDEX ledger_entries_account_idx ON ledger_entries (account_id);

CREATE FUNCTION ledger_reject_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'posted ledger records are immutable' USING ERRCODE = '23514';
END;
$$;
CREATE TRIGGER ledger_accounts_immutable BEFORE UPDATE OR DELETE ON ledger_accounts
    FOR EACH ROW EXECUTE FUNCTION ledger_reject_change();
CREATE TRIGGER ledger_journals_immutable BEFORE UPDATE OR DELETE ON ledger_journals
    FOR EACH ROW EXECUTE FUNCTION ledger_reject_change();
CREATE TRIGGER ledger_entries_immutable BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_reject_change();

CREATE FUNCTION ledger_entry_insert_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE posting_transaction XID8;
BEGIN
    SELECT posting_txid INTO posting_transaction FROM ledger_journals WHERE id = NEW.journal_id;
    IF posting_transaction IS NULL OR posting_transaction <> pg_current_xact_id() THEN
        RAISE EXCEPTION 'entries can only be inserted in the journal posting transaction'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER ledger_entry_insert_guard BEFORE INSERT ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entry_insert_guard();

-- Deferred validation permits journal and entries to be inserted before the
-- payment update, then checks the entire posting at transaction commit.
CREATE FUNCTION validate_approval_journal() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    pay RECORD;
    debit_count INTEGER;
    credit_count INTEGER;
    entry_count INTEGER;
    debit_sum NUMERIC;
    credit_sum NUMERIC;
    valid_debit BOOLEAN;
    valid_credit BOOLEAN;
BEGIN
    SELECT p.merchant_id, p.amount, p.currency, p.status, p.provider_operation_id,
           o.outcome, o.payment_id AS provider_payment_id
      INTO pay
      FROM payments p
      LEFT JOIN simulated_provider_operations o ON o.operation_id = NEW.provider_operation_id
     WHERE p.id = NEW.payment_id;
    IF NOT FOUND OR pay.status <> 'COMPLETED' OR pay.outcome IS DISTINCT FROM 'APPROVED'
       OR pay.provider_payment_id IS DISTINCT FROM NEW.payment_id
       OR pay.provider_operation_id IS DISTINCT FROM NEW.provider_operation_id
       OR pay.merchant_id IS DISTINCT FROM NEW.merchant_id
       OR pay.currency IS DISTINCT FROM NEW.currency OR pay.amount IS DISTINCT FROM NEW.amount
       OR NEW.posting_reference <> 'approval:' || NEW.payment_id::text THEN
        RAISE EXCEPTION 'approval journal does not match a completed, provider-approved payment'
            USING ERRCODE = '23514';
    END IF;
    SELECT count(*), count(*) FILTER (WHERE e.side = 'DEBIT'),
           count(*) FILTER (WHERE e.side = 'CREDIT'),
           coalesce(sum(e.amount) FILTER (WHERE e.side = 'DEBIT'), 0),
           coalesce(sum(e.amount) FILTER (WHERE e.side = 'CREDIT'), 0),
           bool_and(e.side = 'DEBIT' AND a.kind = 'PROVIDER_RECEIVABLE'
               AND a.merchant_id IS NULL AND a.currency = NEW.currency
               AND e.currency = NEW.currency AND e.amount = NEW.amount)
               FILTER (WHERE e.side = 'DEBIT'),
           bool_and(e.side = 'CREDIT' AND a.kind = 'MERCHANT_PAYABLE'
               AND a.merchant_id = NEW.merchant_id AND a.currency = NEW.currency
               AND e.currency = NEW.currency AND e.amount = NEW.amount)
               FILTER (WHERE e.side = 'CREDIT')
      INTO entry_count, debit_count, credit_count, debit_sum, credit_sum, valid_debit, valid_credit
      FROM ledger_entries e JOIN ledger_accounts a ON a.id = e.account_id
     WHERE e.journal_id = NEW.id;
    IF entry_count <> 2 OR debit_count <> 1 OR credit_count <> 1
       OR debit_sum <> credit_sum OR debit_sum <> NEW.amount
       OR valid_debit IS DISTINCT FROM TRUE OR valid_credit IS DISTINCT FROM TRUE THEN
        RAISE EXCEPTION 'approval journal must contain matching balanced debit and credit entries'
            USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END;
$$;
CREATE CONSTRAINT TRIGGER approval_journal_balanced
    AFTER INSERT ON ledger_journals DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION validate_approval_journal();

-- Migration 002's trigger is extended without changing its migration file.
CREATE OR REPLACE FUNCTION enforce_payment_status_transition() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id OR NEW.merchant_id IS DISTINCT FROM OLD.merchant_id
       OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
       OR NEW.request_fingerprint IS DISTINCT FROM OLD.request_fingerprint
       OR NEW.amount IS DISTINCT FROM OLD.amount OR NEW.currency IS DISTINCT FROM OLD.currency THEN
        RAISE EXCEPTION 'payment identity and amount are immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.status <> 'CREATED' AND (
        NEW.provider_operation_id IS DISTINCT FROM OLD.provider_operation_id
        OR NEW.processing_scenario IS DISTINCT FROM OLD.processing_scenario
    ) THEN
        RAISE EXCEPTION 'provider operation and scenario cannot change after processing starts'
            USING ERRCODE = '23514';
    END IF;
    IF NEW.status = OLD.status THEN RETURN NEW; END IF;
    IF NEW.status = 'COMPLETED' AND NOT EXISTS (
        SELECT 1 FROM simulated_provider_operations
        WHERE operation_id = NEW.provider_operation_id AND payment_id = NEW.id AND outcome = 'APPROVED'
    ) THEN
        RAISE EXCEPTION 'COMPLETED requires a confirmed provider approval'
            USING ERRCODE = '23514';
    END IF;
    IF NEW.status = 'COMPLETED' AND NOT EXISTS (
        SELECT 1 FROM ledger_journals
        WHERE payment_id = NEW.id AND purpose = 'PAYMENT_APPROVAL'
          AND provider_operation_id = NEW.provider_operation_id
          AND amount = NEW.amount AND currency = NEW.currency AND merchant_id = NEW.merchant_id
    ) THEN
        RAISE EXCEPTION 'COMPLETED requires an approval journal' USING ERRCODE = '23514';
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

CREATE TABLE operator_actions (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    payment_id UUID REFERENCES payments(id),
    action TEXT NOT NULL CHECK (action IN ('RECONCILE', 'BACKFILL')),
    actor TEXT NOT NULL CHECK (actor <> ''),
    state TEXT NOT NULL CHECK (state IN ('STARTED', 'SUCCEEDED', 'FAILED')),
    before_status TEXT,
    after_status TEXT,
    error_text TEXT,
    requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ
);
CREATE INDEX operator_actions_payment_idx ON operator_actions (payment_id, requested_at DESC);

CREATE FUNCTION guard_operator_action() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'operator audit records cannot be deleted' USING ERRCODE = '23514';
    END IF;
    IF OLD.state <> 'STARTED' OR NEW.id IS DISTINCT FROM OLD.id
       OR NEW.payment_id IS DISTINCT FROM OLD.payment_id
       OR NEW.action IS DISTINCT FROM OLD.action OR NEW.actor IS DISTINCT FROM OLD.actor
       OR NEW.requested_at IS DISTINCT FROM OLD.requested_at
       OR NEW.state NOT IN ('SUCCEEDED', 'FAILED') OR NEW.finished_at IS NULL THEN
        RAISE EXCEPTION 'operator audit record cannot be rewritten' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER operator_actions_guard BEFORE UPDATE OR DELETE ON operator_actions
    FOR EACH ROW EXECUTE FUNCTION guard_operator_action();

-- A recorded definitive provider decision cannot be rewritten after ledger
-- posting. Pending operations may still resolve to a confirmed decision.
CREATE FUNCTION guard_provider_decision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'provider operation records are durable' USING ERRCODE = '23514';
    END IF;
    IF NEW.operation_id IS DISTINCT FROM OLD.operation_id
       OR NEW.payment_id IS DISTINCT FROM OLD.payment_id
       OR NEW.scenario IS DISTINCT FROM OLD.scenario
       OR (OLD.outcome IN ('APPROVED', 'DECLINED') AND
           (NEW.outcome IS DISTINCT FROM OLD.outcome
            OR NEW.logical_charge_id IS DISTINCT FROM OLD.logical_charge_id)) THEN
        RAISE EXCEPTION 'confirmed provider decisions are immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER provider_decision_guard
    BEFORE UPDATE OR DELETE ON simulated_provider_operations
    FOR EACH ROW EXECUTE FUNCTION guard_provider_decision();
