# PAYFLOW

PAYFLOW is a simulated payment creation API written in Go. It stores JPY-denominated payment requests in PostgreSQL and exposes `CreatePayment` and `GetPayment` through ConnectRPC. It does not move money or contact banks.

## Phase 1 behavior

- `CreatePayment` accepts a nonempty merchant ID and idempotency key, a positive integer amount in JPY, and the literal currency `JPY`. Amounts are whole yen (`int64`), never floating point.
- A payment starts and remains `CREATED` in this phase. Repeating the same merchant/key and amount/currency returns the original payment. Reusing the key with a changed valid payload, such as a different amount, returns `ALREADY_EXISTS`. Unsupported currencies are rejected as `INVALID_ARGUMENT` before idempotency lookup.
- `GetPayment` looks up a payment by UUID. Missing IDs return `NOT_FOUND`; malformed IDs return `INVALID_ARGUMENT`.
- Requests are simulated records only. There is no authorization, settlement, refund, ledger, or external provider.

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

In a second PowerShell window, create and fetch a sample payment:

```powershell
$body = '{"merchantId":"demo-merchant","idempotencyKey":"order-1001","amount":"1200","currency":"JPY"}'
Invoke-RestMethod -Uri 'http://127.0.0.1:8080/payflow.v1.PaymentService/CreatePayment' -Method Post -ContentType 'application/json' -Body $body
$id = '<paymentId from the response>'
Invoke-RestMethod -Uri 'http://127.0.0.1:8080/payflow.v1.PaymentService/GetPayment' -Method Post -ContentType 'application/json' -Body "{`"paymentId`":`"$id`"}"
```

Protobuf JSON represents `int64` values as strings, so `"amount":"1200"` is intentional.

## Build, generate, and test

```powershell
go mod download
go build ./...
go test ./...
go vet ./...
gofmt -l (rg --files -g '*.go')
buf lint
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
| `internal/payment` | Validation, fingerprinting, service, RPC adapter |
| `internal/storage` | PostgreSQL persistence through pgx |
| `proto/payflow/v1` | Protobuf API contract |
| `gen/go` | Generated Protobuf and Connect code |
| `db/migrations` | Versioned SQL and migration runner |
| `tests` | PostgreSQL integration tests |

See [architecture](docs/architecture.md) for concurrency and extension notes.

## License

MIT. See [LICENSE](LICENSE).
