# PAYFLOW

PAYFLOW is a simulated payment backend written in Go. It stores JPY-denominated payment requests in PostgreSQL and orchestrates a durable mock banking provider through Temporal. It does not move money or contact banks.

## AWS readiness (Phase 6)

The repository now includes three multi-stage Linux container targets (`api`, `worker`, `migrate`), a production configuration for RDS hostname-verified TLS and Temporal Cloud API-key TLS, and Terraform for a private AWS ECS Fargate deployment. Terraform defines private API/worker tasks, isolated RDS, an internal HTTPS ALB, ECR, Secrets Manager secret containers, IAM roles, CloudWatch logs and basic alarms. Services begin at zero desired tasks until migrations and historical backfill are reviewed. The local Docker Compose workflow below remains the development environment.

**No AWS resources are provisioned by this repository or CI.** Deployment requires an approved private client route, certificate, remote Terraform state, Temporal Cloud namespace, application database user, and a private administration path for credentials and backfill. The existing local-only administration commands intentionally cannot be run against RDS. Production disables the simulation methods and retains merchant bearer authentication; the current production API supports Create/Get/Balance while a separately authorized processing trigger remains future work.

See [AWS architecture](docs/aws-architecture.md), [PowerShell deployment runbook](docs/deployment.md), and [cost categories](docs/costs.md). These documents describe prerequisites, staging, validation, teardown, and limits. No real banking integration is included.

## Payment lifecycle

- `CreatePayment` derives a nonempty merchant ID from the API credential and accepts a nonempty idempotency key, a positive integer amount in JPY, and the literal currency `JPY`. Amounts are whole yen (`int64`), never floating point.
- A payment starts `CREATED`. Local simulation claims it as `PROCESSING`, then records `COMPLETED` only after a confirmed mock approval or `FAILED` only after a confirmed mock decline. An uncertain result becomes `RECONCILIATION_REQUIRED`; a timeout never means failure.
- `RECONCILIATION_REQUIRED` can be claimed for reconciliation and return to `PROCESSING`. Reconciliation queries the durable provider record, then retries only with the same operation ID when the result is still unknown.
- Repeating the same merchant/key and amount/currency returns the original payment, including its current status. Reusing the key with a changed valid payload, such as a different amount, returns `ALREADY_EXISTS`. Unsupported currencies are rejected as `INVALID_ARGUMENT` before idempotency lookup.
- `GetPayment` looks up an owned payment by UUID. Missing or other merchants' IDs return `NOT_FOUND`; malformed IDs return `INVALID_ARGUMENT`.
- `SimulatePayment` and `ReconcilePayment` are **development-only** operations. They are disabled by default and require a loopback listening address. Do not expose this API on a public interface. `SimulatePayment` persists a workflow request and returns the current payment; processing is asynchronous. `ReconcilePayment` remains an immediate manual recovery operation.
- All operations are simulated records only. There is no settlement, refund, or external provider.

## Merchant credentials and isolation

Every ConnectRPC request requires `Authorization: Bearer pfk_...`. A credential belongs to one merchant. Keys contain 256 random bits; migration 005 stores only their SHA-256 hashes. `CreatePayment` derives the merchant from the key. Its legacy `merchant_id` field may be omitted; a mismatched value returns `PERMISSION_DENIED`. Payment lookup, simulation, and reconciliation hide other merchants' payments as `NOT_FOUND`. `GetMerchantBalance` returns only the authenticated merchant's JPY payable balance. Workers and the local operator use internal database access and are not HTTP merchant endpoints.

Provision, rotate, and revoke keys with the loopback-only local command. It prints each new secret once; keep the output out of source control and logs:

```powershell
go run ./cmd/credentials issue demo-merchant
go run ./cmd/credentials rotate <key_id>
go run ./cmd/credentials revoke <key_id>
```

Rotation atomically issues a successor and revokes the old key. Existing payment and idempotency rows are untouched. Development simulation stays disabled unless enabled on a loopback API listener; an authenticated merchant can simulate only its own payment.

## Double-entry ledger and upgrade

A confirmed mock approval posts one immutable `PAYMENT_APPROVAL` journal: debit **Simulated Provider Receivable** and credit the approving merchant's **Merchant Payable** account by the same whole-yen amount. The provider receivable is an unsettled simulated claim, not bank funds. A decline or unknown outcome posts nothing. The journal and `COMPLETED` status commit in one PostgreSQL transaction. Database triggers require the provider approval, payment match, two positive entries, balanced amount, correct accounts, and an approval journal before a new completion. A unique `(payment_id, purpose)` constraint prevents duplicate postings under retries or concurrent activity calls.

Migration 004 creates the ledger without changing Phase 1–3 payments. Older `COMPLETED` payments initially appear in `operator issues`; they remain intact until a separate backfill verifies each durable provider approval. Run the backfill **after migrating and before starting the API and worker** on an upgraded database:

```powershell
$env:DATABASE_URL = 'postgres://payflow:payflow@localhost:5432/payflow?sslmode=disable'
go run ./cmd/migrate
go run ./cmd/operator issues
go run ./cmd/operator backfill --confirm
go run ./cmd/operator issues
```

Backfill is safe to rerun after interruption. It stops and reports an inconsistent historical payment without inventing an approval; already posted journals remain unique. The operator command writes audit rows for mutation attempts.

## Requirements and selected versions

Go 1.27.2, PostgreSQL 17.6 (Compose image), Temporal development server image `temporalio/temporal:1.9.1`, Temporal Go SDK v1.49.0, Buf CLI 1.73.0, `protoc-gen-go` v1.36.12, `protoc-gen-connect-go` v2.0.0, ConnectRPC v2.0.0, pgx v5.11.0, and OpenTelemetry Go v1.44.0. `go.mod` and `go.sum` lock library versions. The generated Go code is committed; Buf and its plugins are needed only to regenerate it.

Phase 6 also uses Terraform CLI 1.14.9 and pins the AWS provider to 6.64.0 with a committed lock file. The Dockerfile pins the Go 1.27.2 Bookworm builder and distroless Debian 12 non-root runtime by image digest. The checked-in RDS global CA bundle is a public trust anchor, not a credential.

## Local setup (Windows PowerShell)

Install Go and Buf, and start Docker Desktop. From the repository root:

```powershell
docker compose up -d --wait
Copy-Item .env.example .env
$env:DATABASE_URL = 'postgres://payflow:payflow@localhost:5432/payflow?sslmode=disable'
go run ./cmd/migrate
go run ./cmd/operator issues
go run ./cmd/operator backfill --confirm
$keyOutput = go run ./cmd/credentials issue demo-merchant
$apiKey = ($keyOutput | Where-Object { $_ -like 'api_key=*' }) -replace '^api_key=', ''
$apiKey # Copy this once into your separate API client shell; do not commit it.
$env:PAYFLOW_ENABLE_DEV_PROCESSING = 'true'
go run ./cmd/api
```

In a second PowerShell window, start the worker:

```powershell
$env:DATABASE_URL = 'postgres://payflow:payflow@localhost:5432/payflow?sslmode=disable'
go run ./cmd/worker
```

The API listens on `127.0.0.1:8080`; Temporal gRPC listens on `127.0.0.1:7233` and its UI is at `http://127.0.0.1:8233`. `.env` is an example for your shell or tooling; the Go programs read environment variables directly.

Omit `PAYFLOW_ENABLE_DEV_PROCESSING` to keep simulation disabled. The server refuses this setting when `LISTEN_ADDR` is not loopback. `TEMPORAL_ADDRESS`, `TEMPORAL_NAMESPACE`, and `TEMPORAL_TASK_QUEUE` are configurable in both processes. The API passes `PAYFLOW_RECONCILE_INTERVAL` (default `2s`), `PAYFLOW_RECONCILE_MAX_INTERVAL` (`30s`), and `PAYFLOW_RECONCILE_WINDOW` (`2m`) into each new workflow.

In a second PowerShell window, create and fetch a sample payment:

```powershell
$body = '{"merchantId":"demo-merchant","idempotencyKey":"order-1001","amount":"1200","currency":"JPY"}'
$headers = @{ Authorization = 'Bearer <api_key from provisioning output>' }
Invoke-RestMethod -Uri 'http://127.0.0.1:8080/payflow.v1.PaymentService/CreatePayment' -Method Post -ContentType 'application/json' -Headers $headers -Body $body
$id = '<paymentId from the response>'
Invoke-RestMethod -Uri 'http://127.0.0.1:8080/payflow.v1.PaymentService/GetPayment' -Method Post -ContentType 'application/json' -Headers $headers -Body "{`"paymentId`":`"$id`"}"
```

Protobuf JSON represents `int64` values as strings, so `"amount":"1200"` is intentional.

### Reproduce simulated outcomes

With local simulation enabled, use a new idempotency key for each example:

```powershell
$base = 'http://127.0.0.1:8080/payflow.v1.PaymentService'
function New-DemoPayment($key) {
  $body = @{merchantId='demo-merchant'; idempotencyKey=$key; amount='1200'; currency='JPY'} | ConvertTo-Json -Compress
  (Invoke-RestMethod -Uri "$base/CreatePayment" -Method Post -ContentType 'application/json' -Headers $headers -Body $body).payment.paymentId
}
function Invoke-DemoScenario($id, $scenario) {
  $body = @{paymentId=$id; scenario=$scenario} | ConvertTo-Json -Compress
  Invoke-RestMethod -Uri "$base/SimulatePayment" -Method Post -ContentType 'application/json' -Headers $headers -Body $body
}
function Invoke-DemoReconcile($id) {
  $body = @{paymentId=$id} | ConvertTo-Json -Compress
  Invoke-RestMethod -Uri "$base/ReconcilePayment" -Method Post -ContentType 'application/json' -Headers $headers -Body $body
}

$approved = New-DemoPayment 'approved-1'
Invoke-DemoScenario $approved 'SIMULATION_SCENARIO_APPROVE'             # queued; eventually COMPLETED
$declined = New-DemoPayment 'declined-1'
Invoke-DemoScenario $declined 'SIMULATION_SCENARIO_DECLINE'             # queued; eventually FAILED
$uncertain = New-DemoPayment 'timeout-1'
Invoke-DemoScenario $uncertain 'SIMULATION_SCENARIO_NETWORK_TIMEOUT'    # eventually RECONCILIATION_REQUIRED
$lost = New-DemoPayment 'lost-response-1'
Invoke-DemoScenario $lost 'SIMULATION_SCENARIO_LOST_APPROVAL_RESPONSE'  # uncertain, then COMPLETED
$temporary = New-DemoPayment 'temporary-1'
Invoke-DemoScenario $temporary 'SIMULATION_SCENARIO_TEMPORARY_FAILURE'  # uncertain, then COMPLETED
Invoke-DemoScenario $approved 'SIMULATION_SCENARIO_APPROVE'             # same workflow and charge
$get = @{paymentId=$approved} | ConvertTo-Json -Compress
Invoke-RestMethod -Uri "$base/GetPayment" -Method Post -ContentType 'application/json' -Headers $headers -Body $get
```

Poll `GetPayment` for the eventual status. The Temporal UI shows each workflow as `payflow-payment-<paymentId>`. Stop and restart the worker while a workflow waits to see it resume. If Temporal is unavailable, `SimulatePayment` returns `UNAVAILABLE` after saving the dispatch request; the worker scans pending requests every two seconds after recovery. A duplicate `SimulatePayment` does not issue another provider operation. A scenario change returns `ALREADY_EXISTS`. After the reconciliation window, the worker continues bounded recovery attempts. A permanently unknown timeout stays `RECONCILIATION_REQUIRED` for manual investigation; `ReconcilePayment` remains available locally.

### Local operator recovery

The operator is a local Go command, not an HTTP endpoint. It accepts only a loopback PostgreSQL address. Inspection is read-only; recovery and backfill require `--confirm` and are recorded in `operator_actions`. Recovery refuses active leases and terminal payments. It queries the original provider operation through the existing processor and cannot force an unknown outcome to `COMPLETED` or `FAILED`.

```powershell
$env:DATABASE_URL = 'postgres://payflow:payflow@localhost:5432/payflow?sslmode=disable'
go run ./cmd/operator inspect $approved
go run ./cmd/operator actions $approved
go run ./cmd/operator stuck
go run ./cmd/operator unresolved
go run ./cmd/operator issues
go run ./cmd/operator balance-provider JPY
go run ./cmd/operator balance-merchant demo-merchant JPY
go run ./cmd/operator recover $uncertain --confirm
```

`stuck` lists expired `PROCESSING` leases, including claims left after a prolonged PostgreSQL outage and an expired Temporal workflow. The worker now scans these claims and unresolved payments automatically. `recover ID --confirm` remains an audited manual override; rerunning it cannot create another provider operation or journal. A still-unknown payment remains `RECONCILIATION_REQUIRED`.

## Health, metrics, and automatic recovery

`GET /healthz` reports process liveness. `GET /readyz` checks PostgreSQL and, when relevant, Temporal. The API exposes these on its listen address. The worker exposes both on `127.0.0.1:9091` by default. API metrics are at `127.0.0.1:9090/metrics`; worker metrics are at `127.0.0.1:9091/metrics`. Metrics listeners refuse non-loopback addresses. They use fixed metric names without merchant, payment, key, or provider-operation labels. The Go services emit structured JSON logs with request, payment, merchant, workflow, activity, operation, duration, and error-category fields where relevant; credentials and connection strings are excluded. OpenTelemetry meter instruments mirror the local Prometheus text metrics. The default local SDK reader does not export remotely.

```powershell
Invoke-WebRequest 'http://127.0.0.1:8080/healthz'
Invoke-WebRequest 'http://127.0.0.1:8080/readyz'
(Invoke-WebRequest 'http://127.0.0.1:9090/metrics').Content
(Invoke-WebRequest 'http://127.0.0.1:9091/metrics').Content
go run ./cmd/operator stuck
go run ./cmd/operator unresolved
go run ./cmd/operator issues
```

The worker scans every `PAYFLOW_RECOVERY_INTERVAL` (default `10s`). It reserves due rows with PostgreSQL `FOR UPDATE SKIP LOCKED`, increments a durable retry count, and releases the transaction before reconciling. Each retry uses the original provider operation ID and claim-token checks. Exponential backoff is capped by `PAYFLOW_RECOVERY_MAX_INTERVAL` (default `5m`); `PAYFLOW_RECOVERY_MAX_ATTEMPTS` (default `8`) bounds automatic retries. After that, an unresolved payment needs local operator investigation. A database outage delays scanning; it does not infer a decline. Database reconnection allows the next scan to resume. `PAYFLOW_REQUEST_TIMEOUT` defaults to `15s`; the API and worker metrics addresses and retry settings are shown in `.env.example`.

Use the Temporal UI at `http://127.0.0.1:8233` to inspect `payflow-payment-<paymentId>`, the JSON worker log for its `workflow_id` and `activity_name`, and `operator inspect <paymentId>` for the durable provider decision. [Local alert rules](docs/alerts.yml) cover reconciliation backlog, dispatch failures, stuck claims, ledger integrity, and sustained database errors. They are examples for a loopback Prometheus scrape; no notification integration is installed.

Development Compose credentials are only for the local container. Supply a secret-managed PostgreSQL URL, TLS, private metrics ingress, and authenticated operator access for any deployment outside a single-machine demonstration. The local credential command and simulation endpoints are intentionally unsuitable for public exposure.

## Build, generate, and test

```powershell
go mod download
go build ./...
go test -count=1 ./...
go vet ./...
gofmt -l (rg --files -g '*.go')
buf lint
buf generate
```

`go test -count=1 ./...` runs unit tests and skips PostgreSQL or Temporal integration tests when their dependency environment variables are unset. Use a disposable test database for integration tests:

```powershell
docker compose exec postgres createdb -U payflow payflow_test
$env:TEST_DATABASE_URL = 'postgres://payflow:payflow@localhost:5432/payflow_test?sslmode=disable'
$env:TEST_TEMPORAL_ADDRESS = '127.0.0.1:7233'
go test -count=1 ./...
```

The integration tests apply migrations to `payflow_test` and leave test rows there. To recreate the test database, use a fresh Compose volume or drop and create the database while no tests are running. Temporal's local SQLite state persists in the Compose `payflow_temporaldata` volume.

To regenerate protobuf and Connect code, install the pinned plugins and put Go's binary directory on PATH:

```powershell
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
go install connectrpc.com/connect/v2/cmd/protoc-gen-connect-go@v2.0.0
$env:PATH = "$(go env GOPATH)\bin;$env:PATH"
buf generate
go mod tidy
```

The checked-in `buf.gen.yaml` uses local plugins to avoid depending on a hosted generator service. The versioned SQL migrations are embedded in `cmd/migrate`, so `go run ./cmd/migrate` can run from any working directory.

## Repository map

| Path | Purpose |
| --- | --- |
| `cmd/api` | ConnectRPC HTTP server |
| `cmd/worker` | Temporal worker and pending dispatch scanner |
| `cmd/operator` | Local inspection, backfill, and audited recovery |
| `cmd/credentials` | Local merchant key provisioning, rotation, and revocation |
| `cmd/migrate` | Embedded SQL migration runner |
| `cmd/healthcheck` | Loopback HTTP readiness probe used by containers |
| `internal/config` | Shared database configuration and production TLS requirements |
| `infrastructure/terraform` | Private AWS deployment definition and example inputs |
| `.github/workflows` | CI checks without deployment |
| `internal/payment` | Validation, lifecycle, processing service, RPC adapter |
| `internal/banking` | Durable local mock provider and scenarios |
| `internal/storage` | PostgreSQL persistence through pgx |
| `internal/orchestration` | Workflow, activity, and dispatch coordinator |
| `internal/ledger` | Transactional approval posting and read-only ledger queries |
| `internal/operator` | Local recovery and audit service |
| `internal/auth` | Hashed merchant credentials and identity context |
| `internal/recovery` | Expired-claim and unresolved-outcome scanner |
| `internal/observability` | Structured correlation context and bounded metrics |
| `proto/payflow/v1` | Protobuf API contract |
| `gen/go` | Generated Protobuf and Connect code |
| `db/migrations` | Versioned SQL and migration runner |
| `tests` | PostgreSQL integration and recovery tests |

See [architecture](docs/architecture.md) for concurrency and extension notes.

## License

MIT. See [LICENSE](LICENSE).
