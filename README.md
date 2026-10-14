# PAYFLOW

PAYFLOW is a simulated payment backend written in Go. It stores JPY-denominated payment requests in PostgreSQL, then exercises a durable mock banking provider through ConnectRPC. It does not move money or contact banks.

## Payment lifecycle

- `CreatePayment` accepts a nonempty merchant ID and idempotency key, a positive integer amount in JPY, and the literal currency `JPY`. Amounts are whole yen (`int64`), never floating point.
- A payment starts `CREATED`. Local simulation claims it as `PROCESSING`, then records `COMPLETED` only after a confirmed mock approval or `FAILED` only after a confirmed mock decline. An uncertain result becomes `RECONCILIATION_REQUIRED`; a timeout never means failure.
- `RECONCILIATION_REQUIRED` can be claimed for reconciliation and return to `PROCESSING`. Reconciliation queries the durable provider record, then retries only with the same operation ID when the result is still unknown.
- Repeating the same merchant/key and amount/currency returns the original payment, including its current status. Reusing the key with a changed valid payload, such as a different amount, returns `ALREADY_EXISTS`. Unsupported currencies are rejected as `INVALID_ARGUMENT` before idempotency lookup.
- `GetPayment` looks up a payment by UUID. Missing IDs return `NOT_FOUND`; malformed IDs return `INVALID_ARGUMENT`.
- `SimulatePayment` and `ReconcilePayment` are **development-only** operations. They are disabled by default. Enabling them requires a loopback listening address. Do not expose this API on a public interface.
- All operations are simulated records only. There is no authorization, settlement, refund, ledger, or external provider.

## Requirements and selected versions

Go 1.27.2, PostgreSQL 17.6 (Compose image), Buf CLI 1.73.0, `protoc-gen-go` v1.36.12, `protoc-gen-connect-go` v2.0.0, ConnectRPC v2.0.0, and pgx v5.11.0. `go.mod` and `go.sum` lock library versions. The generated Go code is committed; Buf and its plugins are needed only to regenerate it. Go 1.27 is the supported toolchain for this project.

## Local setup (Windows PowerShell)

Install Go and Buf, and start Docker Desktop. From the repository root:

```powershell
docker compose up -d --wait
Copy-Item .env.example .env
$env:DATABASE_URL = 'postgres://payflow:payflow@localhost:5432/payflow?sslmode=disable'
go run ./cmd/migrate
go run ./cmd/api
```

The API listens on `127.0.0.1:8080` by default. `.env` is an example for your shell or tooling; the Go programs read environment variables directly. To choose another address, set `$env:LISTEN_ADDR`.

To enable local simulation, set `$env:PAYFLOW_ENABLE_DEV_PROCESSING = 'true'` before `go run ./cmd/api`. The server refuses this setting when `LISTEN_ADDR` is not loopback. Run `go run ./cmd/migrate` after pulling Phase 2 changes; migration 002 preserves Phase 1 payments and idempotency keys.

In a second PowerShell window, create and fetch a sample payment:

```powershell
$body = '{"merchantId":"demo-merchant","idempotencyKey":"order-1001","amount":"1200","currency":"JPY"}'
Invoke-RestMethod -Uri 'http://127.0.0.1:8080/payflow.v1.PaymentService/CreatePayment' -Method Post -ContentType 'application/json' -Body $body
$id = '<paymentId from the response>'
Invoke-RestMethod -Uri 'http://127.0.0.1:8080/payflow.v1.PaymentService/GetPayment' -Method Post -ContentType 'application/json' -Body "{`"paymentId`":`"$id`"}"
```

Protobuf JSON represents `int64` values as strings, so `"amount":"1200"` is intentional.

### Reproduce simulated outcomes

With local simulation enabled, use a new idempotency key for each example:

```powershell
$base = 'http://127.0.0.1:8080/payflow.v1.PaymentService'
function New-DemoPayment($key) {
  $body = @{merchantId='demo-merchant'; idempotencyKey=$key; amount='1200'; currency='JPY'} | ConvertTo-Json -Compress
  (Invoke-RestMethod -Uri "$base/CreatePayment" -Method Post -ContentType 'application/json' -Body $body).payment.paymentId
}
function Invoke-DemoScenario($id, $scenario) {
  $body = @{paymentId=$id; scenario=$scenario} | ConvertTo-Json -Compress
  Invoke-RestMethod -Uri "$base/SimulatePayment" -Method Post -ContentType 'application/json' -Body $body
}
function Invoke-DemoReconcile($id) {
  $body = @{paymentId=$id} | ConvertTo-Json -Compress
  Invoke-RestMethod -Uri "$base/ReconcilePayment" -Method Post -ContentType 'application/json' -Body $body
}

$approved = New-DemoPayment 'approved-1'
Invoke-DemoScenario $approved 'SIMULATION_SCENARIO_APPROVE'             # COMPLETED
$declined = New-DemoPayment 'declined-1'
Invoke-DemoScenario $declined 'SIMULATION_SCENARIO_DECLINE'             # FAILED
$uncertain = New-DemoPayment 'timeout-1'
Invoke-DemoScenario $uncertain 'SIMULATION_SCENARIO_NETWORK_TIMEOUT'    # RECONCILIATION_REQUIRED
Invoke-DemoReconcile $uncertain                                        # still uncertain
$lost = New-DemoPayment 'lost-response-1'
Invoke-DemoScenario $lost 'SIMULATION_SCENARIO_LOST_APPROVAL_RESPONSE'  # uncertain
Invoke-DemoReconcile $lost                                             # COMPLETED
$temporary = New-DemoPayment 'temporary-1'
Invoke-DemoScenario $temporary 'SIMULATION_SCENARIO_TEMPORARY_FAILURE'  # uncertain
Invoke-DemoReconcile $temporary                                        # COMPLETED
```

Restart the API between simulation and reconciliation to exercise durable recovery. A duplicate `SimulatePayment` call returns the existing payment and does not issue another provider operation. A scenario change on the same payment returns `ALREADY_EXISTS`.

## Build, generate, and test

```powershell
go mod download
go build ./...
go test ./...
go vet ./...
gofmt -l (rg --files -g '*.go')
buf lint
buf generate
```

`go test ./...` runs unit tests and skips PostgreSQL integration tests when `TEST_DATABASE_URL` is unset. Use a disposable test database for integration tests:

```powershell
docker compose exec postgres createdb -U payflow payflow_test
$env:TEST_DATABASE_URL = 'postgres://payflow:payflow@localhost:5432/payflow_test?sslmode=disable'
go test -count=1 ./tests
```

The integration test applies migrations to `payflow_test` and leaves test rows there. To recreate the test database, use a fresh Compose volume or drop and create the database while no tests are running.

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
| `cmd/migrate` | Embedded SQL migration runner |
| `internal/payment` | Validation, lifecycle, processing service, RPC adapter |
| `internal/banking` | Durable local mock provider and scenarios |
| `internal/storage` | PostgreSQL persistence through pgx |
| `proto/payflow/v1` | Protobuf API contract |
| `gen/go` | Generated Protobuf and Connect code |
| `db/migrations` | Versioned SQL and migration runner |
| `tests` | PostgreSQL integration and recovery tests |

See [architecture](docs/architecture.md) for concurrency and extension notes.

## License

MIT. See [LICENSE](LICENSE).
