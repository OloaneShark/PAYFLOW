# Local simulated payment performance

All figures and commands here are for **LOCAL SIMULATED PAYMENT BENCHMARKS**. They do not measure a bank, AWS, or production capacity. The [reliability guide](reliability-testing.md) covers process and container interruption.

## Test design and safety

`cmd/loadtest` uses the generated ConnectRPC v2 client and a normal bearer credential. It issues a synthetic merchant key only in a dedicated local PostgreSQL database, then sends bounded concurrent requests to the real API. It attempts to revoke that key when the run ends; a prolonged database outage can delay cleanup in the disposable database. The key and database URL are never written to the result files. The API must run with `PAYFLOW_LOADTEST_MODE=true`, `PAYFLOW_ENV=development`, a loopback listener, and a database named `payflow_loadtest_*`. Its `/healthz` response advertises that database name. The client requires `-allow-local-test`, a matching loopback API, `PAYFLOW_LOADTEST_DATABASE_URL` on loopback **port other than 5432**, and the matching health marker. It refuses production mode and remote endpoints.

`compose.loadtest.yaml` uses its own Compose project, PostgreSQL and Temporal volumes, and host ports 15432/17233/18233. The ordinary development stack and database are separate. Request limits are 1–5000, clients 1–64, duration at most five minutes, and each request has a deadline. The `stress` preset, more than 1000 requests, more than 16 clients, or a duration over two minutes requires `-stress-opt-in`. No run creates unbounded goroutines or an unbounded request queue.

| Workload | Measured request pattern |
| --- | --- |
| `create` | Unique authenticated `CreatePayment` calls |
| `get` | Repeated owned `GetPayment` calls after a setup creation |
| `submit` | Distinct precreated payments submitted once each |
| `repeat` | Concurrent identical creates with the same merchant and idempotency key |
| `concurrent-submit` | Concurrent submissions of one precreated payment |
| `mixed` | Create, get, submit, and merchant balance calls in rotation |
| `balance` | Authenticated JPY merchant balance queries |

Setup requests are excluded from the measured API request count and latency. Each result includes actual synchronous API latency, ConnectRPC error categories, throughput, and a separate database observation of asynchronous dispatch and completion latency. `COMPLETED` latency includes only approved payments and runs from payment creation to completion. Dispatch latency runs from durable queue insertion to dispatch confirmation. Reconciliation latency is reported only for records that passed through `RECONCILIATION_REQUIRED`. A successful `SubmitPayment` response alone does **not** count as payment completion. The client waits up to `-settle-timeout`; pending submitted payments make a normal benchmark incomplete. Individual database operation and ledger posting latency are not instrumented, so those fields are explicitly unavailable.

The post-run verifier checks every synthetic merchant payment against the durable provider operation, logical charge ID, approval journal, debit/credit totals, account roles, currency, posting reference, merchant payable balance, and terminal status. It checks same-key and same-payment workloads for one payment or workflow ID. PostgreSQL constraints and existing tests separately enforce journal immutability. A benchmark cannot pass with an invariant violation, request failure, or pending submitted payment.

## Windows PowerShell setup

Install Go 1.27.2 and start Docker Desktop. From the repository root, use a new disposable stack:

```powershell
docker compose -f compose.loadtest.yaml up -d --wait
$env:DATABASE_URL = 'postgres://payflow:payflow@127.0.0.1:15432/payflow_loadtest_smoke?sslmode=disable'
$env:PAYFLOW_LOADTEST_DATABASE_URL = $env:DATABASE_URL
$env:PAYFLOW_ENV = 'development'
$env:PAYFLOW_LOADTEST_MODE = 'true'
$env:LISTEN_ADDR = '127.0.0.1:18080'
$env:PAYFLOW_METRICS_ADDR = '127.0.0.1:19090'
$env:PAYFLOW_WORKER_METRICS_ADDR = '127.0.0.1:19091'
$env:TEMPORAL_ADDRESS = '127.0.0.1:17233'
go run ./cmd/migrate
New-Item -ItemType Directory -Force .tools | Out-Null
go build -o .tools/phase8-api.exe ./cmd/api
go build -o .tools/phase8-worker.exe ./cmd/worker
go build -o .tools/phase8-loadtest.exe ./cmd/loadtest
```

Run the API and worker in separate PowerShell windows with the same environment values. In the API window, run `.\.tools\phase8-api.exe`; in the worker window, run `.\.tools\phase8-worker.exe`. Once `/healthz` returns `X-Payflow-Loadtest-Database: payflow_loadtest_smoke`, run a benchmark from a third window with `PAYFLOW_ENV` and `PAYFLOW_LOADTEST_DATABASE_URL` set:

```powershell
.\.tools\phase8-loadtest.exe -allow-local-test -preset smoke -workload mixed
.\.tools\phase8-loadtest.exe -allow-local-test -preset baseline -workload mixed
.\.tools\phase8-loadtest.exe -allow-local-test -preset smoke -workload repeat -requests 64 -clients 8
.\.tools\phase8-loadtest.exe -allow-local-test -preset smoke -workload concurrent-submit -requests 64 -clients 8
# Manually initiated only:
foreach ($clients in @(4, 8, 16, 32)) {
  .\.tools\phase8-loadtest.exe -allow-local-test -stress-opt-in -preset stress -workload mixed -clients $clients
}
```

Use `-requests`, `-clients`, `-duration`, `-timeout`, `-settle-timeout`, `-workload`, `-format json|csv|markdown|all`, `-api`, and `-output-dir` to change a run within the hard limits. Presets are smoke 20/2, baseline 200/8, stress 1000/16, and recovery 50/4 requests/clients. Each run writes JSON, CSV, and/or Markdown beneath ignored `results/loadtest/`. Do not commit raw results without reviewing them. To stop the isolated containers after the API and worker have exited:

```powershell
docker compose -f compose.loadtest.yaml down
```

`down` retains the dedicated volumes. Removing volumes is a separate destructive decision and is not part of benchmark cleanup.

## Observed local results

Observed October 18, 2026 (America/New_York), on Windows/amd64 with Go 1.27.2, 28 logical CPUs, PostgreSQL 17.6, and the configured Temporal development image `temporalio/temporal:1.9.1`. Temporal's runtime version was not queried. The generated artifacts remain local and ignored by Git.

| Run | Requests | API failures | Measured requests/s | API p95 | Average dispatch | Average approval completion | Invariant violations |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Smoke mixed | 20 | 0 | 719.8 | 5.24 ms | 1,849 ms | 1,906 ms | 0 |
| Baseline mixed | 200 | 0 | 2,679.2 | 5.43 ms | 1,670 ms | 1,908 ms | 0 |
| Concurrent submit | 64 | 0 | 2,349.4 | 5.27 ms | 1,369 ms | 1,407 ms | 0 |
| Repeated create | 64 | 0 | 2,068.9 | 16.96 ms | — | — | 0 |

These small, local, bursty runs are comparisons within one machine, not capacity claims. The repeated-key p95 was higher than mixed traffic; unique-key contention is a plausible explanation, but no lock-wait profile was captured, so the cause is unconfirmed. Approval completion was dominated by waiting for worker dispatch. The worker scans the durable queue every two seconds; changing that interval would trade database polling load for lower dispatch latency. There is no evidence here that a code optimization is justified.

An `EXPLAIN (ANALYZE, BUFFERS)` of the pending-dispatch query on this disposable database used `payment_workflow_dispatch_pending_idx` and reported 0.021 ms execution time with **zero pending rows**. That confirms the idle query plan, not its performance under a deep backlog. Eight-client baseline traffic showed zero request failures; database pool pressure, lock waits, worker saturation, retry excess, and server goroutine growth were not independently measured. The result records the load client's Go heap system allocation, not server memory. Capture `pg_stat_activity`, server profiles, and a sustained backlog before making throughput changes.

No performance-sensitive application code was changed in this phase, so there is no before/after optimization claim. Longer stress runs were not executed automatically.
