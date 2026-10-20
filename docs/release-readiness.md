# PAYFLOW release-candidate verification

**Assessment date:** October 20, 2026 (America/New_York)  
**Candidate:** `main` at `96c4d836dacce1d9ff7d8143af276799c1d1a4fc`  
**Status:** **READY FOR DEPLOYMENT PREPARATION**

This status authorizes no deployment. It means the committed local simulation passed the available release checks and can enter the separately authorized preparation process in [deployment.md](deployment.md). AWS configuration, identity, connectivity, cost approval, and an actual deployment remain unverified. PAYFLOW moves no real money.

## Source and GitHub Actions

At the start of this assessment, local `main`, `origin/main`, and GitHub `main` all pointed to the candidate SHA. `git status --porcelain=v1 --untracked-files=all` was empty. The commit contains all 21 Phase 9 changed files, including the load-test tooling, alerts, preflight, temporary deployment, and teardown guidance. This document is an uncommitted Phase 10 addition, so the working tree is no longer clean after the assessment. No commit or push was made.

GitHub Actions [CI run 39356983254](https://github.com/OloaneShark/PAYFLOW/actions/runs/39356983254) completed successfully for this exact SHA. Its three jobs passed:

| Job | Verified steps |
| --- | --- |
| `go` | Format, build, PostgreSQL and Temporal integration suite, vet, Buf lint, generated-code comparison |
| `docker` | API, worker, migrator, and private administrator image builds |
| `terraform` | Format, local initialization without backend, validation |

The separately dispatched `performance.yml` workflow has **no run associated with this SHA** in the commit's available Actions results. Its Linux CGO race suite, isolated smoke benchmark, and Prometheus rule checks are therefore **unverified in GitHub Actions for this candidate**. Previous local Phase 9 evidence includes a Linux Docker race suite and Prometheus rule tests; those are not GitHub Actions results.

## Local preflight and functional review

Ran `docker compose -f compose.loadtest.yaml up -d --wait`, then `scripts/preflight.ps1` with `TEST_DATABASE_URL` set to the isolated loopback `payflow_loadtest_smoke` database on port 15432 and `TEST_TEMPORAL_ADDRESS=127.0.0.1:17233`. PostgreSQL reported healthy and Temporal was running. The integration suite uses the environment variables, applies forward migrations, and connects to the disposable PostgreSQL and Temporal services. The preflight exited **0**.

| Check | Result |
| --- | --- |
| Git cleanliness at preflight start; Go formatting, build, tests, vet | PASS |
| Disposable PostgreSQL and Temporal integration tests | PASS |
| Buf lint and generated Protobuf SHA-256 comparison | PASS |
| Terraform format, `init -backend=false`, validate | PASS |
| Docker daemon and all four production image targets | PASS |
| Ignored `terraform.tfvars` | NOT CONFIGURED |
| AWS identity, route, certificate, state, secrets, IAM roles, Temporal Cloud | NOT TESTED |
| Actual AWS deployment readiness | NOT TESTED |

Local versions observed: Go 1.27.2 on Windows/amd64, Buf 1.73.0, Terraform 1.14.9, Docker Engine 29.5.3. Preflight neither contacted AWS nor ran Terraform plan/apply/destroy. The four image builds prove buildability, not deployment or registry publication. The isolated Compose volumes were retained; stopping the containers does not remove test data.

The targeted code and test review found no confirmed high-priority defect requiring a payment, ledger, or infrastructure change:

| Area | Verified design and evidence | Remaining limit |
| --- | --- | --- |
| Merchant authentication and isolation | Bearer keys are random, stored as SHA-256 digests, checked before ConnectRPC routing; handlers derive merchant identity from the credential and check payment ownership. Credential and tenant-isolation integration tests are present and passed. | Production key issuance, delivery, revocation, and IAM/database grants require external setup. |
| Payment idempotency | PostgreSQL uniquely constrains merchant plus idempotency key; repeated payloads return the original row, while changed payloads conflict. Concurrent same-key tests passed. | Guarantees depend on the configured PostgreSQL database remaining durable. |
| Durable Temporal dispatch | `SubmitPayment` stores a workflow request before the worker contacts Temporal. Workflow IDs derive from payment IDs; pending rows are retried after dispatch errors. Temporal retry and worker-restart tests passed. | Temporal Cloud connectivity and operation were not tested. |
| Provider outcome and ledger | One stable provider operation ID per payment survives retries. Approval requires a confirmed provider row. Completion and a balanced, immutable two-entry journal commit together; decline and unknown outcomes do not post. Duplicate and rollback tests passed. | Historical migration/backfill review remains a deployment task; this is a simulated ledger. |
| Recovery | Expired claims and reconciliation-required rows are retried without treating a timeout as a decline. Tests cover lost approval, unknown outcome, database update failure, reconnection, and concurrent recovery. | Retry exhaustion needs operator action; no AWS interruption was tested. |
| Private administration and secrets | Production disables development simulation. The one-off admin command allowlist has no HTTP route; new keys are delivered to Secrets Manager without task output, and startup secrets are referenced by ARN. | Existing invoker and recipient roles, secure delivery, populated secret versions, and CloudTrail attribution require account review. |
| Images and infrastructure | Four pinned-base, nonroot distroless image targets built. Terraform describes an internal TLS ALB, private tasks and RDS, encrypted storage, deletion protection, and immutable image digests. Local validation passed. | No plan against real inputs, AWS resource inspection, image publication, or runtime smoke test occurred. |

## Existing performance and reliability evidence

No benchmark or fault injection was rerun for this phase because the existing short-run evidence is sufficient for functional regression review and no performance-sensitive code changed. The Phase 9 local simulated mixed smoke had 20 requests, zero API failures, zero invariant violations, zero pending submissions, 694.5 requests/s, and 5.73 ms API p95. Three 200-request baseline trials with eight clients measured 2,646.4, 2,518.1, and 2,270.8 requests/s; all had zero API failures, invariant violations, and pending submissions. A separate 20-request sample observed 3 of 3 terminal Temporal histories with 280.6 ms average workflow execution. These short runs do not establish sustained throughput or AWS capacity. See [performance.md](performance.md) for setup, per-trial latency, and measurement limits.

Phase 8 single-run local interruptions covered API restart, worker restart, PostgreSQL interruption, and Temporal unavailability with 40 submitted payments per run, zero final pending payments, and zero invariant violations. Phase 9 repeated only the API restart with the improved timing harness; its last payment became terminal 4.290 seconds after injection and 1.992 seconds after restoration. These are observations, not recovery objectives. See [reliability-testing.md](reliability-testing.md).

### Prepared sustained run — authorization required

The existing `sustained` preset is prepared but **was not run**. One proposed local trial uses `-allow-local-test -stress-opt-in -preset sustained -workload mixed -requests 5000 -clients 8 -duration 2m -warmup 10s -settle-timeout 45s`. It permits at most 5,000 measured requests, paced at about 42 starts/second over two minutes, after an unmeasured ten-second warmup. Each request has a five-second timeout. The run then allows up to 45 seconds for submitted payments to settle and verifies provider and ledger invariants. It needs a loopback API and worker, the isolated PostgreSQL 17.6 and Temporal development containers, and local CPU/memory/disk for those processes and retained volumes. It uses no paid service or AWS resource. Execute it only after the user's explicit authorization; its result must be reported separately and must not be inferred from the baseline runs.

## Remaining deployment prerequisites and safeguards

Before any separately authorized deployment, record the AWS account/region and operator identities; review the private client route, DNS and matching certificate, two availability zones, encrypted versioned remote Terraform state and locking, actual Terraform plan, three distinct PostgreSQL roles/grants, populated startup secrets, Temporal Cloud endpoint and billing owner, approved admin invoker and credential recipient roles, four reviewed image digests, migration exit code, historical ledger review, private authenticated smoke transaction, and monitoring/alert routing. The application Prometheus rules need a configured scraper, probe, and receiver; local syntax tests do not deliver production alerts.

Obtain a current account-specific cost estimate and budget alert. The [temporary deployment worksheet](temporary-deployment.md) calls for a named deployment and teardown owner, a 24-hour stop time, and separate approval for any extension up to 48 hours. Two NAT gateways, Multi-AZ RDS, an internal ALB, Fargate, CloudWatch, Secrets Manager, ECR, retained snapshots, state, and Temporal Cloud can accrue charges even when services are scaled to zero. The [teardown checklist](teardown.md) requires inventory verification, careful RDS deletion-protection/final-snapshot handling, secret and credential retirement, external Temporal cleanup, and residual billing review. No AWS resources were created and no teardown was performed in this phase.

**Final assessment: READY FOR DEPLOYMENT PREPARATION.** The candidate passed local preflight and exact-commit CI. Deployment remains conditional on the external prerequisites above and separate authorization. Sustained load remains pending explicit authorization.
