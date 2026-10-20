# Local deployment preflight

PAYFLOW is a simulated payment system. This page records local checks for a possible separately authorized, time-boxed AWS demonstration. Local validation does **not** prove AWS connectivity or deployment readiness. No Phase 9 step provisions AWS resources or runs `terraform plan`, `apply`, or `destroy`.

Run from Windows PowerShell:

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\preflight.ps1
```

The script reports `PASS`, `FAIL`, `NOT CONFIGURED`, or `NOT TESTED` for the Go toolchain, formatting, build, unit tests, vet, disposable integration test configuration, Buf lint and generated-code consistency, Terraform formatting/init/validate, Docker daemon and four image targets, Git cleanliness, and deployment inputs. `-SkipDockerBuilds` makes the image targets `NOT TESTED` when only a quick inspection is wanted. It reads tool output internally without printing connection strings or credentials. It never calls AWS. Terraform initialization uses `-backend=false`; the local provider cache and ignored `.terraform` directory may be populated.

Integration checks run only when both `TEST_DATABASE_URL` and `TEST_TEMPORAL_ADDRESS` point to loopback services and the PostgreSQL database name starts with `payflow_loadtest_` on a non-5432 port. The script does not create, stop, or remove a database or container. Without those variables, the general `go test` suite may skip integration tests and the separate integration line reads `NOT CONFIGURED`. Start the isolated stack and set the variables before preflight:

```powershell
docker compose -f compose.loadtest.yaml up -d --wait
$env:TEST_DATABASE_URL = 'postgres://payflow:payflow@127.0.0.1:15432/payflow_loadtest_smoke?sslmode=disable'
$env:TEST_TEMPORAL_ADDRESS = '127.0.0.1:17233'
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\preflight.ps1
```

The integration suite applies versioned migrations and writes synthetic records to the disposable database. The script verifies generated Protobuf files by generating into a temporary ignored directory and comparing SHA-256 hashes. It does not rewrite `gen/go`.

## External deployment gate

The following require separate review and authorized checks; preflight labels them `NOT TESTED` or `NOT CONFIGURED`:

- AWS identity and account/region, private client route, private DNS, ACM certificate, two approved availability zones, and encrypted versioned S3 state with locking.
- A current AWS Pricing Calculator estimate and budget alert for the selected 24-hour window, with a named teardown owner and 48-hour extension approver.
- Temporal Cloud namespace, endpoint, API key, connectivity, and its independent billing owner.
- Three separate RDS database roles and reviewed grants, populated startup secret versions, two existing IAM roles for credential delivery and private administration, and a controlled merchant-key delivery path.
- Four immutable image digests, migration task exit code, historical ledger review, API/worker health, private authenticated smoke transaction, alert routing, and operator recovery access.

The full staging sequence is in [deployment.md](deployment.md). Record the external gate before any separate deployment authorization; a clean Terraform validation or local preflight is only evidence of local configuration syntax and tests.

## Observed local preflight (October 19, 2026)

With the isolated Compose stack and integration variables above, Go formatting/build/tests/vet, PostgreSQL and Temporal integration tests, Buf lint and generated-file hash comparison, Terraform fmt/init with the cached provider/validate, Docker daemon, and all four container builds reported `PASS`. The working tree reported `FAIL` because Phase 9 changes are uncommitted; this is an accurate development-state result, not a code-test failure. The ignored `terraform.tfvars` was absent (`NOT CONFIGURED`). AWS identity, private routing, state, secrets, roles, Temporal Cloud, and actual deployment readiness remained `NOT TESTED`. The overall preflight exited nonzero because the working tree was dirty. No GitHub Actions run or AWS call was made by this preflight.

## Phase 9 correctness review

Reviewed the authentication middleware and merchant ownership checks, local-only simulation controls, private admin task command allowlist and secret handling, idempotent creation, durable workflow IDs, provider operation reuse, transactional journal completion, cancellation and readiness paths, and Terraform production defaults. Existing integration tests cover duplicate dispatch, failed dispatch retry, worker restart, database reconnection, lost provider response, permanently unknown outcome, concurrent posting, and rollback of interrupted journal completion. No confirmed financial-semantics defect required an application transaction change in this phase. Added operational gauges and an ECS worker task alarm; production delivery of application Prometheus gauges remains an external monitoring prerequisite.
