# Phase 1 architecture

The API server mounts generated ConnectRPC v2 handlers on `net/http`. The handler translates Protobuf requests to the payment service and maps domain errors to explicit Connect codes. The service validates fields, generates a random UUIDv4, and hashes the amount and currency into a SHA-256 request fingerprint. The PostgreSQL adapter uses pgxpool, context-aware operations, and parameterized SQL.

`payments` has a primary key on `id` and a unique constraint on `(merchant_id, idempotency_key)`. Creation runs `INSERT ... ON CONFLICT DO NOTHING RETURNING ...`. If another request won the key, the adapter issues a separate `SELECT` and compares the stored fingerprint and payload. The unique index serializes competing inserts; the second statement gets a fresh PostgreSQL `READ COMMITTED` snapshot after a competing transaction commits. Thus concurrent identical requests converge on one row, while a changed payload receives a conflict. The service generates an unused UUID before the insert; losing contenders simply discard theirs.

The fingerprint covers amount and currency, which are all mutable request payload fields in Phase 1. Merchant ID and idempotency key form the lookup scope. Exact string equality is used for merchant IDs and keys; no whitespace normalization is performed beyond rejecting all-whitespace values. The database stores the fingerprint and original payload for defense against accidental mismatches. A future API field added to creation must also be included in the fingerprint before release.

Amount is an integer count of whole yen. Only `JPY` is accepted. The database enforces positive amounts and stores `BIGINT`, so no floating-point money enters the system. The schema currently permits only `CREATED`, deliberately preventing unsupported state transitions. A later processing phase can add status values in a new migration when it has simulated outcomes to record.

The migration runner embeds numbered `.up.sql` files, records applied versions in `schema_migrations`, and takes a PostgreSQL advisory lock so two runners cannot apply a migration at once. Each version runs in one transaction. Matching `.down.sql` files document reversals; this runner only applies forward migrations to avoid accidental data loss.

Tests cover validation in memory and API behavior against a separate PostgreSQL test database through Connect's in-process transport. An HTTP test also exercises the mounted Connect route and Protobuf JSON encoding. The concurrency test starts 24 calls together against the same merchant/key and checks that all return one payment ID.

## Assumptions and limits

- Merchant identity is supplied by the caller. Authentication, authorization, and merchant tenancy enforcement are outside Phase 1.
- The API has no processing worker and never marks a payment `COMPLETED` or `FAILED`.
- Local Compose credentials are development-only. Deployment security and operational hardening are future work.
- Idempotency persists as long as the payment row exists. No expiration or deletion path exists in Phase 1.

## Future phases

The service/store boundary leaves room for a Temporal workflow to coordinate simulated processing. A separate internal double-entry ledger can record balanced entries after a simulated outcome. A mock banking service can return deterministic approvals and declines. These are design directions only; none are implemented here.
