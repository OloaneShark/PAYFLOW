# Deployment preparation and runbook (Windows PowerShell)

This runbook is for a future, authorized **simulation** deployment. Phase 6 validation does not run `terraform apply`, start ECS tasks, create an AWS resource, or access a real AWS account. Review the architecture and cost notes before provisioning.

## Prerequisites

- AWS account and least-privilege deployment identity; AWS CLI v2, Terraform 1.14.9, Docker Desktop, Go 1.27.2, and Buf 1.73.0 installed. Terraform pins AWS provider 6.64.0 in `versions.tf` and `.terraform.lock.hcl`.
- Pre-existing, secured S3 state bucket with versioning and encryption; configure the S3 backend with native lockfiles. Keep backend configuration and state out of Git. This Terraform module does not provision its own backend.
- Two availability zones in the chosen region, a routed VPN/private client network, a private DNS name, and an ACM certificate for that name. Set `private_client_cidr` to only the authorized client network. Confirm the ALB hostname routing and certificate match before access.
- Independently provisioned Temporal Cloud namespace, endpoint, and API key. Store the key only in Secrets Manager.
- An approved private RDS administration process to create a least-privilege application user and provision/rotate merchant API keys. The repository's `cmd/credentials` and `cmd/operator` intentionally refuse a remote database. This is a **deployment blocker** until that private process exists and any historical ledger backfill is reviewed.

## Local validation

From the repository root:

```powershell
gofmt -l (rg --files -g '*.go')
go build ./...
go test -count=1 ./...
go vet ./...
buf lint
buf generate
git diff --exit-code -- gen/go
docker build --target api -t payflow-api:local .
docker build --target worker -t payflow-worker:local .
docker build --target migrate -t payflow-migrate:local .
terraform -chdir=infrastructure/terraform fmt -check -recursive
terraform -chdir=infrastructure/terraform init -backend=false -input=false
terraform -chdir=infrastructure/terraform validate
```

For PostgreSQL and Temporal integration tests, keep using local Compose. Use a new disposable test database so its schema comes from the current migrations:

```powershell
docker compose up -d --wait
docker compose exec postgres createdb -U payflow payflow_phase6_test
$env:TEST_DATABASE_URL = 'postgres://payflow:payflow@localhost:5432/payflow_phase6_test?sslmode=disable'
$env:TEST_TEMPORAL_ADDRESS = '127.0.0.1:7233'
go test -count=1 ./...
```

## Future authorized rollout sequence

1. Create the approved S3 remote state bucket and private connectivity outside this module. Copy `infrastructure/terraform/terraform.tfvars.example` to ignored `terraform.tfvars` and replace the placeholders. Leave `enable_runtime=false`, `api_desired_count=0`, and `worker_desired_count=0`. Never place secret values in tfvars.
2. Initialize the S3 backend with your bucket, key, region, encryption, and lockfile settings; run `terraform plan` and review network, RDS, IAM, and costs. Apply only after deployment authorization. This creates empty application secret containers, ECR repositories, RDS, and the internal ALB. It does not start API or worker tasks.

   ```powershell
   Copy-Item infrastructure/terraform/terraform.tfvars.example infrastructure/terraform/terraform.tfvars
   terraform -chdir=infrastructure/terraform init -backend-config='bucket=<approved-state-bucket>' -backend-config='key=payflow/demo.tfstate' -backend-config='region=us-east-1' -backend-config='encrypt=true' -backend-config='use_lockfile=true'
   terraform -chdir=infrastructure/terraform plan -var-file=terraform.tfvars -out=payflow.tfplan
   # After a separate deployment approval: terraform -chdir=infrastructure/terraform apply payflow.tfplan
   ```

   The ignored plan and state files can contain infrastructure metadata. Secure them and do not share them as build artifacts.
3. Via the approved private database administration workflow, create separate runtime and migration database roles. The runtime role needs only the application data access needed by API/worker; the migration role needs schema ownership and DDL privileges. Populate `database-runtime-url` and `database-migration-url` with their respective PostgreSQL URLs using the RDS hostname and `sslmode=verify-full&sslrootcert=/etc/ssl/certs/rds-global-bundle.pem`. Use secure secret entry, not a Terraform variable, source file, log, or command history. Populate the Temporal API key secret the same way. Confirm the Temporal namespace and endpoint.
4. Build the three Docker targets for Linux/amd64, tag them uniquely, and push each to its Terraform-created ECR repository through an authorized registry workflow. Record each immutable image's **repository digest**, not merely a mutable tag. Set `enable_runtime=true`, Temporal address/namespace, and the three digest variables. Keep desired counts zero. Review a second plan and apply to register task definitions and zero-count services.
5. Run the `migrate` task definition once in an application subnet using `migration_security_group`, `assignPublicIp=DISABLED`, and the Terraform output cluster ARN. Wait for the task's stopped state, verify the migrator container exited **0**, and review its CloudWatch log stream. A task launch response is not migration success. The migration runner uses PostgreSQL advisory locking and embedded forward migrations. Do not scale API/worker if it fails.
6. On databases upgraded from before migration 004, inspect historical `COMPLETED` rows and execute the existing provider-verified backfill through the approved private administration process. Run `operator issues` equivalent after backfill. Do not infer approval from the status alone. The local loopback-only `cmd/operator` is not an ECS task and is not to be repointed at RDS.
7. Only after migration, backfill review, credentials, connectivity, and readiness prerequisites are complete, set `migrations_complete=true`, then set desired counts to at least one and review/apply the plan. Check ECS task health, ALB `/readyz`, CloudWatch logs, RDS alarms, Temporal worker poller health, and authenticated Create/Get/Balance through the private client path. Development processing methods must reject calls.

For an ECS migration invocation, prefer a reviewed organization runbook or the AWS console using the task definition and network IDs in Terraform outputs. Do not use the API service security group for the migrator; it has a separate one. Pin the same migrator digest that was validated in CI.

## Changes, recovery, and rollback

- Deploy a new task definition by pinning a new digest and reviewing a Terraform plan. Keep previous immutable ECR images for rollback. ECS rollout should be observed through readiness and CloudWatch logs.
- A failed migration blocks scaling. Versioned migrations are forward-only in this deployment; restore from a verified RDS snapshot through a separate recovery procedure if a destructive migration is ever introduced. Do not automatically run down migrations against production data.
- Temporal timeouts and lost responses preserve uncertain payment state and stable provider operation IDs. Restarting ECS tasks does not imply failure or a new logical charge. Use the existing recovery scanner and verified operator workflow.
- Updating a Secrets Manager value alone does not refresh a running ECS container. Roll tasks after a credential rotation and verify readiness.
- No public endpoint, simulation API, remote admin task, or metrics listener is enabled by this deployment.

## Monitoring and troubleshooting

CloudWatch has separate `/ecs/payflow-<environment>/api`, `/worker`, and `/migrate` log groups. Logs are structured JSON for API/worker; correlate by request, payment, workflow, and provider operation identifiers. Do not paste database URLs, API keys, or merchant bearer credentials into incident tickets. The internal ALB target health reflects API `/readyz`; worker ECS health reflects PostgreSQL and Temporal readiness. Review RDS free storage and CPU alarms, ECS service events, task stop reasons, and Temporal Cloud worker polling when a task is unhealthy.

- **API cannot become ready:** verify the runtime secret contains a correct private RDS DNS hostname, verify-full TLS settings, CA path, and runtime role grants; then inspect RDS/SG connectivity and API logs.
- **Worker cannot become ready:** check the above database path plus Temporal Cloud endpoint, namespace, API key secret, outbound 7233 route, and TLS certificate validity. Restart tasks after secret changes.
- **Migration task stopped nonzero:** inspect only its CloudWatch log stream and migration table; fix the cause before attempting a rerun. The advisory lock serializes concurrent runners, but a failed migration still blocks service scaling.
- **ALB target unhealthy:** check `/readyz`, target group port 8080, API SG ingress from ALB, and ECS task health. A private ALB also requires a working client route and matching private DNS/certificate for user access.
- **Payment remains uncertain:** retain `RECONCILIATION_REQUIRED` or an expired claim until the durable provider operation can be verified. Use the bounded worker scanner and approved private operator workflow; do not classify a timeout as a decline.

## Teardown after explicit authorization

Stop client traffic, take an approved database backup/export, set API and worker desired counts to zero, and verify all ECS tasks have stopped. Review retained CloudWatch logs, ECR images, Secrets Manager recovery windows, RDS snapshots, and the S3 remote state lifecycle. RDS deletion protection must be deliberately disabled in a reviewed change before destroy, and a final snapshot must be retained. A Terraform destroy is an irreversible separate decision; never run it as a validation step. Remove private DNS, certificate, VPN routing, Temporal Cloud assets, and state storage only through their owners' procedures.
