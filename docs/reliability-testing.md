# Local reliability and failure injection

PAYFLOW's existing PostgreSQL and Temporal integration suite already tests duplicate creation, conflicting idempotency payloads, concurrent payment processing and ledger posting, stable provider operation IDs, lost approval response recovery, permanently unknown outcomes, worker restart, Temporal start failure, and database reconnection. Phase 8 adds an independent financial verifier and a manual process/container interruption harness. All testing uses synthetic payments and the local mock provider.

## Run the integration and race suites

Start the disposable stack in [performance setup](performance.md), then run from PowerShell:

```powershell
$env:TEST_DATABASE_URL = 'postgres://payflow:payflow@127.0.0.1:15432/payflow_loadtest_smoke?sslmode=disable'
$env:TEST_TEMPORAL_ADDRESS = '127.0.0.1:17233'
go test -count=1 ./...
go test -count=1 ./tests -run 'TestTemporalDispatchRecoveryAndWorkerRestart|TestLostApprovalResponseReconcilesAfterRestart|TestRecoveryUnknownOutcomeAndDatabaseReconnection|TestConcurrentProcessingBalanceAndFinancialAudit'
```

The selected tests prove the original provider operation survives lost responses and restarts, one approved payment posts one journal, decline/unknown payments post none, and a permanently unknown result remains available for operator investigation. Existing database triggers reject journal modification and invalid completion. The Phase 8 verifier reads persisted data after concurrent processing and compares the merchant payable balance with completed payment amounts.

Windows Go may have CGO disabled. A Linux container with Go 1.27.2 and CGO enabled can run the full race suite against only the disposable Compose network:

```powershell
docker run --rm --network payflow-loadtest_default `
  -v "${PWD}:/src" -v payflow-race-mod:/go/pkg/mod -v payflow-race-cache:/root/.cache/go-build `
  -w /src -e CGO_ENABLED=1 `
  -e TEST_DATABASE_URL='postgres://payflow:payflow@postgres:5432/payflow_loadtest_smoke?sslmode=disable' `
  -e TEST_TEMPORAL_ADDRESS='temporal:7233' `
  golang:1.27.2-bookworm go test -race -count=1 ./...
```

This Linux race command completed successfully locally on October 18, 2026, including PostgreSQL and Temporal integration tests. It is also available through the manually triggered `.github/workflows/performance.yml` workflow. A passing race run checks only paths exercised by its tests and is not a proof that all possible interleavings are race-free.

## Manually injected failures

The PowerShell harness operates only on the fixed `compose.loadtest.yaml` project and the API/worker processes it starts. It refuses production mode, checks that port 18080 is free, requires `-AllowLocalFailure`, and restores a stopped isolated container in `finally`. It does not stop normal development containers or remove volumes. It creates 40 synthetic payments, submits them through the authenticated API, waits for durable dispatch, injects one selected failure, and verifies terminal outcomes and financial invariants in a JSON result. Use one scenario at a time after stopping any manually launched API/worker on port 18080:

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\reliability.ps1 -Scenario api-restart -AllowLocalFailure
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\reliability.ps1 -Scenario worker-restart -AllowLocalFailure
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\reliability.ps1 -Scenario postgres-interruption -AllowLocalFailure
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\reliability.ps1 -Scenario temporal-unavailable -AllowLocalFailure
```

An API restart leaves already committed dispatches available to the worker. A worker restart resumes durable Temporal history and pending dispatches. A PostgreSQL interruption delays reads and processing; it cannot imply a payment failure. A Temporal outage leaves dispatch rows pending until the worker reconnects. The harness waits up to 90 seconds for terminal results. If it cannot prove recovery, it fails; inspect ignored `results/loadtest/*.json` and `.tools/reliability/*.stderr` rather than assuming success. Container restore is attempted even after an error.

Lost provider approval and permanently unknown outcomes are covered by deterministic integration tests using `LOST_APPROVAL_RESPONSE` and `NETWORK_TIMEOUT`. The provider stores an approval before losing the response in the former case, so reconciliation looks up the **same** operation ID and posts one journal. A timeout in the latter case stays `RECONCILIATION_REQUIRED` with no approval journal. Local-only `SimulatePayment` and `ReconcilePayment` can reproduce those cases manually as described in [README](../README.md); they are never exposed by the production API.

## Observed recovery results

Observed October 18, 2026 (America/New_York) on the disposable Windows/Docker stack. Each passing run submitted 40 payments, reported zero API request failures and zero invariant violations, and had zero submitted payments pending at the end. Approval completion timing includes queueing, interruption, and recovery.

| Injected failure | Average dispatch | Average approval completion | Result |
| --- | ---: | ---: | --- |
| API restart | 2.67 s | 3.07 s | Passed |
| Worker restart | 3.72 s | 4.28 s | Passed |
| PostgreSQL interruption | 4.56 s | 5.19 s | Passed |
| Temporal unavailable | 5.90 s | 13.58 s | Passed |

These are single local runs, not reliability service-level objectives. The Temporal interruption had the longest recovery in this sample; worker reconnection and the two-second dispatch scan may contribute, but no detailed reconnect trace was profiled. No external service or AWS resource was used. The intentionally unknown provider case remains unresolved by design and needs operator investigation, not an invented terminal status.

## Phase 9 recovery and alert review

The existing integration tests cover worker restart, Temporal dispatch retry, database reconnection, duplicate workflow requests, concurrent recovery claims, interrupted journal posting, and a lost mock-provider approval response. They assert the original provider operation ID, one logical charge and journal, balanced entries, and unresolved status for a permanently unknown outcome. The manual Phase 8 harness still performs API, worker, PostgreSQL, and Temporal interruption tests in the disposable stack; it records observed dispatch and completion intervals, but each published result is a single run. No Phase 9 sustained outage or AWS failure injection was run.

Phase 9 narrowed the harness's injection precondition to **new** pending dispatches created after the run began; stale rows from earlier tests cannot satisfy it. It also reads PostgreSQL timestamps for injection, service restoration, and the last newly submitted terminal payment. An October 19, 2026 local API restart run submitted 40 payments, had zero API failures, zero pending submissions, and zero invariant violations. Its last payment became terminal 4.290 seconds after injection and 1.992 seconds after API restoration. Those are one-run observations, not a recovery objective. The other three interruption types retain the Phase 8 results above and were not rerun with the new timing output.

The worker now exports `payflow_workflow_dispatch_backlog`, `payflow_reconciliation_retry_exhausted`, and `payflow_database_connectivity` alongside the existing reconciliation, expired-claim, and ledger gauges. The recovery scan refreshes these from durable PostgreSQL rows. Prometheus rules in [alerts.yml](alerts.yml) cover each category plus worker scrape loss and API `/readyz` probe failure. The worker and API `up`/`probe_success` rules require an operator-configured local scrape and blackbox probe; no scraper or notification receiver is deployed by PAYFLOW. CloudWatch has an ALB healthy-target alarm for API readiness and a Container Insights `RunningTaskCount` alarm for worker availability when desired count is positive. Running task count is not proof of Temporal connectivity; the worker container's `/readyz` check exercises PostgreSQL and Temporal.

The local warning thresholds (`>20` dispatch/reconciliation for five/ten minutes, `>0` expired claims for five minutes) are starting assumptions, **not** measured service-level objectives. Retry exhaustion or a ledger violation alerts after two minutes or immediately. Set thresholds from sustained local evidence before an authorized deployment. A database outage can make gauges stale; combine them with scrape loss, `/readyz`, and ECS/RDS health. The `payflow_ledger_integrity_violations` scan examines up to 10,000 completed records per refresh and must not be interpreted as an unbounded full-ledger audit.

To validate rule expressions and important firing delays without altering payment data, use Prometheus `promtool` with synthetic metric series:

```powershell
promtool check rules docs/alerts.yml
promtool test rules docs/alerts.test.yml
```

If `promtool` is not installed, use an official local container (Prometheus v3.7.3). This pulls an image but creates no AWS resource:

```powershell
docker run --rm -v "${PWD}\docs:/etc/payflow:ro" --entrypoint /bin/promtool prom/prometheus:v3.7.3 check rules /etc/payflow/alerts.yml
docker run --rm -v "${PWD}\docs:/etc/payflow:ro" --entrypoint /bin/promtool prom/prometheus:v3.7.3 test rules /etc/payflow/alerts.test.yml
```

Both commands passed locally on October 19, 2026: ten alert expressions loaded and the synthetic firing tests succeeded. This validates PromQL syntax and selected threshold delays, not live AWS metric delivery or notification routing.

To exercise real local conditions, use the isolated stack from [performance.md](performance.md). The `temporal-unavailable` harness scenario leaves durable dispatch rows pending while the local Temporal service is stopped, then restores it; inspect `http://127.0.0.1:19091/metrics` for backlog changes. The `postgres-interruption` scenario can drive database error/connectivity metrics. Use existing deterministic `NETWORK_TIMEOUT` and `LOST_APPROVAL_RESPONSE` integration scenarios for reconciliation. Do not hand-edit ledger or production payments merely to make an alert fire. `promtool` tests cover synthetic threshold behavior without changing infrastructure.
