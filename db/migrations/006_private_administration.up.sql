-- Existing keys remain active. New private-admin keys remain unusable until
-- their Secrets Manager delivery has completed.
ALTER TABLE merchant_api_keys
    ADD COLUMN activated_at TIMESTAMPTZ DEFAULT now(),
    ADD COLUMN admin_request_id UUID UNIQUE;

CREATE TABLE credential_admin_operations (
    request_id UUID PRIMARY KEY,
    action TEXT NOT NULL CHECK (action IN ('ISSUE', 'ROTATE', 'REVOKE')),
    actor TEXT NOT NULL CHECK (actor <> ''),
    merchant_id TEXT,
    target_key_id UUID,
    new_key_id UUID,
    secret_name TEXT,
    secret_arn TEXT,
    state TEXT NOT NULL CHECK (state IN ('PREPARED', 'SUCCEEDED', 'NEEDS_REVIEW')),
    error_category TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    CONSTRAINT credential_admin_completion CHECK (
        (state = 'PREPARED' AND finished_at IS NULL) OR
        (state <> 'PREPARED' AND finished_at IS NOT NULL)
    )
);

ALTER TABLE merchant_api_keys
    ADD CONSTRAINT merchant_api_keys_admin_request_fk
    FOREIGN KEY (admin_request_id) REFERENCES credential_admin_operations(request_id);

CREATE FUNCTION guard_credential_admin_operation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'credential administration records are durable' USING ERRCODE = '23514';
    END IF;
    IF OLD.state <> 'PREPARED' OR NEW.state NOT IN ('SUCCEEDED', 'NEEDS_REVIEW')
       OR NEW.request_id IS DISTINCT FROM OLD.request_id
       OR NEW.action IS DISTINCT FROM OLD.action
       OR NEW.actor IS DISTINCT FROM OLD.actor
       OR NEW.merchant_id IS DISTINCT FROM OLD.merchant_id
       OR NEW.target_key_id IS DISTINCT FROM OLD.target_key_id
       OR NEW.new_key_id IS DISTINCT FROM OLD.new_key_id
       OR NEW.secret_name IS DISTINCT FROM OLD.secret_name
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'credential administration record cannot be rewritten' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER credential_admin_operation_guard
    BEFORE UPDATE OR DELETE ON credential_admin_operations
    FOR EACH ROW EXECUTE FUNCTION guard_credential_admin_operation();
