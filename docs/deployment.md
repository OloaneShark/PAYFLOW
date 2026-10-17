# Private simulation deployment runbook (Windows PowerShell)

This is a preparation guide for a future **authorized** AWS deployment. PAYFLOW does not move money. No `terraform apply`, ECS task, AWS resource, or real account action was run while developing this phase. Review [AWS architecture](aws-architecture.md) and [costs](costs.md) first.

## Prerequisites and ownership

- AWS deployment identity, AWS CLI v2, Terraform 1.14.9, Docker Desktop, Go 1.27.2, Buf 1.73.0, and the versioned Go protobuf plugins. Terraform pins AWS provider 6.64.0.
- Secured S3 remote state bucket with versioning, encryption, and native lockfiles. State and plans stay outside Git.
- Two availability zones, routed private client path, private DNS, and a matching ACM certificate. `private_client_cidr` must be the approved client network.
- Independently provisioned Temporal Cloud namespace, endpoint, and API key.
- Private RDS administration to create **separate** runtime, migration, and administration database roles. The admin role needs the credential tables for issue/rotate/revoke and the existing recovery, audit, payment, provider, and ledger access for operator commands. Review explicit SQL grants against the commands before deploying. Never reuse the RDS master credential in ECS.
- Pre-existing IAM role for the approved credential broker, and a different pre-existing role for admin task invokers. Their trust policies, MFA, account restrictions, and secure merchant credential delivery process must be approved outside Terraform. Terraform only attaches narrow resource policies to these roles.
- An owner for billing, alert routing, log retention, credential rotation, database backup, and incident response.

## Local verification

From the repository root:

```powershell
gofmt -l (rg --files -g '*.go')
go build ./...
go test -count=1 ./...
go vet ./...
buf lint
buf generate
git diff --exit-code -- gen/go  # Use only when generated files were already committed.
docker build --target api -t payflow-api:local .
docker build --target worker -t payflow-worker:local .
docker build --target migrate -t payflow-migrate:local .
docker build --target admin -t payflow-admin:local .
terraform -chdir=infrastructure/terraform fmt -check -recursive
terraform -chdir=infrastructure/terraform init -backend=false -input=false -lockfile=readonly
terraform -chdir=infrastructure/terraform validate
```

`git diff --exit-code -- gen/go` is useful after generation on a committed revision. While developing an uncommitted Protobuf change, compare the generated files with their pre-generation state instead.

Run PostgreSQL and Temporal integration tests against a **disposable** database:

```powershell
docker compose up -d --wait
docker compose exec postgres createdb -U payflow payflow_phase7_test
$env:TEST_DATABASE_URL = 'postgres://payflow:payflow@localhost:5432/payflow_phase7_test?sslmode=disable'
$env:TEST_TEMPORAL_ADDRESS = '127.0.0.1:7233'
go test -count=1 ./...
```

Tests apply forward migrations, including 006. Use a new test database name for a clean run.

## Future authorized rollout

1. Create remote state and private connectivity through approved procedures. Copy `infrastructure/terraform/terraform.tfvars.example` to ignored `terraform.tfvars`; set real region, CIDR, certificate, and availability zones. Leave `enable_runtime=false`, `enable_admin=false`, and desired counts zero. No secret values belong in tfvars, plans, Git, shell command arguments, or task overrides.
2. Initialize the secured remote backend, plan, review network/RDS/IAM/costs, and apply only after authorization. This stage creates infrastructure, ECR repositories, and **empty** startup secret containers. It starts no API or worker service.

   ```powershell
   Copy-Item infrastructure/terraform/terraform.tfvars.example infrastructure/terraform/terraform.tfvars
   terraform -chdir=infrastructure/terraform init -backend-config='bucket=<approved-state-bucket>' -backend-config='key=payflow/demo.tfstate' -backend-config='region=us-east-1' -backend-config='encrypt=true' -backend-config='use_lockfile=true'
   terraform -chdir=infrastructure/terraform plan -var-file=terraform.tfvars -out=payflow.tfplan
   # After separate deployment approval: terraform -chdir=infrastructure/terraform apply payflow.tfplan
   ```

3. Via private DB administration, create runtime, migration, and admin roles with reviewed grants. Fill the corresponding runtime, migration, and admin URL secrets with the RDS hostname and `sslmode=verify-full&sslrootcert=/etc/ssl/certs/rds-global-bundle.pem`. Fill the Temporal API key secret. Use a protected secret-entry process; the module creates containers but no values. Verify TLS, DB connectivity, Temporal namespace, and role grants. Existing local `cmd/credentials` and `cmd/operator` reject remote DB URLs and must not be repointed at RDS.
4. Build and publish the **four** Linux/amd64 Docker targets through an approved registry workflow. Pin each task by its immutable ECR repository digest. Set `enable_runtime=true`, `enable_admin=true`, the four digests, Temporal address/namespace, and the approved recipient/invoker role names. Keep service desired counts zero. Review and apply the second plan to register definitions. The admin task is one-off; no admin service or HTTP route is created.
5. Run the migrator task once with the application subnet IDs, `migration_security_group`, and `assignPublicIp=DISABLED` from Terraform outputs. Wait for it to **stop**, verify container exit code 0, and review its CloudWatch log stream. A successful `RunTask` API response alone says nothing about migration success.
6. On databases upgraded from before migration 004, run `admin issues`, then the provider-verified `admin backfill --confirm`, and re-run `admin issues`. Review historical records rather than treating an old `COMPLETED` label as proof of approval. Migration 006 preserves existing keys as active.
7. Provision merchant credentials with the private admin task. Use a new UUID request ID for each issue or rotation, record that ID in the change ticket, and pass **only IDs** in task command arguments. The task outputs a secret ARN and key ID but no key. The separately approved credential broker reads the new Secrets Manager value and delivers it through its controlled channel. Test the merchant's authenticated Create/Get/Submit path privately. For rotation, confirm the replacement works and the previous key is revoked. For revocation, test the rejected old key. A `NEEDS_REVIEW` operation must be investigated under the **same request ID**; do not casually issue a new one.
8. Confirm the checklist below, set `migrations_complete=true` and `deployment_prerequisites_reviewed=true`, then scale API and worker to at least one in a reviewed plan. Check ECS health, ALB `/readyz`, CloudWatch logs, RDS alarms, Temporal poller health, and authenticated Create/Get/Submit/Balance through the private client path. `SimulatePayment` and `ReconcilePayment` must reject calls. Poll `GetPayment` until an approved or declined mock outcome is recorded; no API-side Temporal connection is required.

For a private admin task, use the `admin_task_definition_arn` and `network_ids` outputs. The task command syntax is:

```text
issue MERCHANT_ID REQUEST_UUID --confirm
rotate OLD_KEY_UUID REQUEST_UUID --confirm
revoke KEY_UUID REQUEST_UUID --confirm
inspect PAYMENT_UUID
actions PAYMENT_UUID
stuck | unresolved | issues
recover PAYMENT_UUID --confirm
backfill --confirm
```

Invoke using `aws ecs run-task` with the reviewed cluster, admin task definition, application subnets, `admin_security_group`, `assignPublicIp=DISABLED`, and a container command override containing only the allowlisted tokens. Use an approved operator wrapper or console to avoid editing arbitrary task settings. Wait for the task to stop, check the `admin` container exit code, and inspect its JSON output. `PAYFLOW_ADMIN_ACTOR` is a generic task label; attribute the human/automation invoker through CloudTrail and the change ticket. Never pass an API key as an ECS override, command argument, log entry, ticket, or Terraform input.

## Readiness checklist

- [ ] **A — Network and identity:** approved private route, DNS/certificate match, ALB CIDR, no public task IP, IAM invoker and broker trust/approval, private admin task authorization.
- [ ] **B — Data and secrets:** Multi-AZ RDS, verified TLS, three distinct database roles and grants, populated startup secrets, migration 006 exit code 0, historical ledger review, backups and access review.
- [ ] **C — Images and workflow:** four scanned/reviewed immutable digests, Temporal Cloud namespace/API key, worker connectivity, admin task definition and secure credential broker, pending dispatch and recovery visibility.
- [ ] **D — Merchant and operation:** issued key delivered by broker, private authenticated Create/Get/Submit/Balance test, rejection of revoked keys, audited admin recovery/backfill, development processing disabled.
- [ ] **E — Operations and cost:** readiness/alarms/logs checked, alert owner, spend estimate and budget, rollback image digests, documented incident and teardown owners.

Terraform's `migrations_complete` and `deployment_prerequisites_reviewed` inputs record a human review only. They do not query AWS or prove each checkbox.

## Failure handling, monitoring, and rollback

`SubmitPayment` commits a dispatch request before returning. If the worker, Temporal, or database becomes unavailable, the pending row or original provider operation remains durable for retry. A returned `CREATED` status means asynchronous processing is pending. A timeout never proves a decline. Worker recovery reuses the original operation ID; approved outcomes still require one balanced journal in the same transaction as `COMPLETED`. Keep uncertain payments unresolved for investigation if the provider record cannot be determined.

Credential issue and rotation use a two-stage delivery. A task interruption or ambiguous Secrets Manager result may leave an inactive key and an orphaned secret. Check the audit row and secret name privately before deciding whether to recover or retire that request; do not activate a key from status alone. The old rotation key stays active until a delivered successor activates. A database outage after secret delivery also requires review. The broker should verify secret access and delivery before exposing the credential to a merchant.

CloudWatch separates `/ecs/payflow-<environment>/api`, `/worker`, `/migrate`, and `/admin` logs. Correlate by payment, workflow, provider operation, and admin request ID. Avoid pasting URLs or credentials into tickets. API `/readyz` checks DB connectivity; worker `/readyz` also checks Temporal. Review ECS events, task stop reasons, RDS storage/CPU alarms, Temporal pollers, and dispatch backlog. No notification destination or external metrics scraper is installed.

For an application rollback, pin a prior reviewed image digest and observe readiness. Schema migrations are forward-only; never run a down migration automatically. A destructive data rollback requires an approved RDS snapshot procedure. Updating a startup secret does not change running ECS containers; roll tasks and verify readiness after secret rotation. Never mark unknown payments failed merely to clear a queue.

Teardown is a separate authorized action: stop traffic and tasks, retain approved backups and final RDS snapshot, review Secrets Manager recovery windows and credential retirement, then handle Terraform destroy and external DNS, VPN, Temporal, and state through their respective owners. Destroy is never a validation step.
