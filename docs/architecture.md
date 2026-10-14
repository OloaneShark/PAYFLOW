# PAYFLOW architecture

## Request path and lifecycle

The Go API mounts generated ConnectRPC v2 handlers on `net/http`. `CreatePayment` validates JPY integer amounts and uses the Phase 1 unique `(merchant_id, idempotency_key)` constraint. An identical request returns the same row even after processing; a changed valid payload conflicts. `GetPayment` returns the stored status and timestamps.

The development-only `SimulatePayment` handler calls the processing service. It atomically changes `CREATED` to `PROCESSING`, assigning one stable `provider_operation_id`, a scenario, a claim token, and a 30-second lease. Another request sees the existing payment and does not call the provider. The service makes the mock provider call after the claim statement commits; it holds no payment transaction open during the call. A confirmed approval permits `PROCESSING → COMPLETED`; a confirmed decline permits `PROCESSING → FAILED`. Any unknown result permits `PROCESSING → RECONCILIATION_REQUIRED`.

`ReconcilePayment` claims a `RECONCILIATION_REQUIRED` payment, or a `PROCESSING` payment whose lease has expired after an interrupted worker. It first looks up the provider operation by its original ID. A recorded decision is applied without another provider execution. If the result is pending or absent, it may retry with that same ID. A new operation ID is never allocated during reconciliation. A still-unknown result returns to `RECONCILIATION_REQUIRED`.

Legal status changes are `CREATED → PROCESSING`, `PROCESSING → COMPLETED|FAILED|RECONCILIATION_REQUIRED`, and `RECONCILIATION_REQUIRED → PROCESSING`. Terminal states cannot transition. Go checks requested changes, conditional SQL enforces claim ownership, and a PostgreSQL trigger rejects invalid status changes or changed operation IDs even from another database client. The trigger also requires a matching persisted provider approval or decline before accepting a terminal status. If the final payment update fails, the lease expires and reconciliation retrieves the provider decision.

## Durable mock provider

`internal/banking` is a separate Go package behind a provider interface. Its PostgreSQL table `simulated_provider_operations` has one row per stable operation ID and one per payment. An approved row has one `logical_charge_id` equal to the operation ID. Uniqueness constraints and provider upserts ensure duplicate or concurrent attempts never create a second logical charge. This is only a simulation; the charge ID is a test artifact, not money movement.

| Scenario | First execution | Reconciliation |
| --- | --- | --- |
| `APPROVE` | Persists approval; payment completes | Reads approval |
| `DECLINE` | Persists decline; payment fails | Reads decline |
| `TEMPORARY_FAILURE` | Persists pending, reports temporary error; payment becomes uncertain | Retries same ID, persists approval, completes |
| `NETWORK_TIMEOUT` | Persists pending, reports timeout; payment becomes uncertain | Remains pending and uncertain |
| `LOST_APPROVAL_RESPONSE` | Persists approval but reports lost response; payment becomes uncertain | Reads approval and completes |

The provider table and payment table share the local PostgreSQL dependency but use separate statements. This intentionally models a provider decision that can outlive failure to update the payment record. All scenario decisions and attempt counts survive process restart.

## Migrations and compatibility

Migration 001 is unchanged. Forward migration 002 expands the payment status constraint, adds operation IDs, leases and lifecycle timestamps, installs transition enforcement, and creates the provider operation table. Existing rows remain `CREATED` with null processing metadata. The embedded migration runner records applied versions in `schema_migrations`, takes an advisory lock, and runs each version in a transaction. `.down.sql` files document reversal, but the runner applies only forward migrations to avoid accidental data loss.

The Protobuf contract keeps Phase 1 methods and field numbers. It adds lifecycle values and development-only simulation/reconciliation methods. The handler maps the actual stored status to Protobuf. The simulation methods are disabled by default and may be enabled only while the API binds to loopback; there is no authentication or authorization layer.

## Limits and future work

- Only JPY whole-yen amounts are accepted. There is no money movement, external banking connection, settlement, refund, or ledger.
- The mock timeout scenario deliberately remains unresolved. No terminal state is guessed from a timeout. An operator or a future workflow could continue reconciliation.
- Recovery of an interrupted `PROCESSING` payment waits for its 30-second claim lease. There is no background reconciliation worker in Phase 2; the local reconciliation RPC must be called.
- A later phase can add a Temporal workflow, an internal double-entry ledger, and a separate mock banking service. None is implemented here.
