# Domain-free temporary AWS demonstration

This is a **future, separately authorized** private backend demonstration. The local preparation of this runbook creates no AWS resource, ACM import, certificate key, Temporal Cloud account, or paid service. PAYFLOW uses synthetic merchants and a PostgreSQL-backed mock provider; no funds move. The standard [deployment gates](deployment.md), [cost worksheet](temporary-deployment.md), and [teardown checklist](teardown.md) still apply.

## Design and trust boundary

Keep the existing two-AZ VPC, two NAT gateways, Multi-AZ RDS, internal HTTPS ALB, Fargate API/worker and one-off migrator/admin tasks, Secrets Manager, CloudWatch, and Temporal Cloud integration. For the demo, set `private_client_cidr = null` (its default) and `enable_demo_access = true`. Terraform adds one private Amazon Linux 2023 `t3.micro` managed access instance in the application subnet, three single-AZ SSM interface endpoints (`ssm`, `ssmmessages`, `ec2messages`), an instance profile, and a policy on an approved existing operator role. The access host has no public IP, SSH key, inbound rule, RDS rule, or general internet egress. Its security group can reach only the internal ALB and SSM endpoint group on 443. The ALB accepts this host security group on 443; its existing API-only egress remains. A `private_client_cidr` can still be set if an independently approved routed client network exists.

The operator uses SSM `AWS-StartPortForwardingSessionToRemoteHost` to forward local `127.0.0.1:18443` through the managed instance to the **internal ALB DNS name** on 443. The approved role's policy grants this document on the one temporary instance and requires document access checking; its trust policy and MFA remain account-owned. The supplied AWS document accepts a host parameter, so the instance security group is the important destination boundary. Verify the access host's SSM Agent is online and version **3.3.4851.0 or newer** before use. AWS's [remote-host forwarding security advisory](https://github.com/aws/amazon-ssm-agent/security/advisories/GHSA-w9jw-h72g-6hxc) identifies earlier versions as vulnerable; replace/update the host before opening a tunnel if this gate fails. Port-forwarded payloads are not recorded in Session Manager session logs; rely on CloudTrail for session API events and PAYFLOW's sanitized application logs for requests. Do not use or modify JobFinitum.

`payflow-demo.test` is a reserved test hostname, so it needs no purchase or public DNS. The local Go smoke client uses that hostname for SNI and name verification while dialing the loopback tunnel. No hosts-file change or Route 53 zone is needed. The ALB presents a short-lived, locally generated self-signed certificate imported into **regional ACM outside Terraform**. The certificate must have a SAN for `payflow-demo.test`, server-auth use, and a matching private key. The client loads the public demo certificate as its trust anchor and leaves Go's TLS verification enabled. ACM does not renew imported certificates. Never put a private key in Git, Terraform variables/state, screenshots, or tickets.

AWS documentation: [SSM remote-host forwarding](https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-sessions-start.html), [SSM private endpoints](https://docs.aws.amazon.com/systems-manager/latest/userguide/setup-create-vpc.html), [ACM self-signed import prerequisites](https://docs.aws.amazon.com/acm/latest/userguide/import-certificate-prerequisites.html), [imported certificate renewal](https://docs.aws.amazon.com/acm/latest/userguide/import-certificate.html). The `.test` reservation is specified in [RFC 6761](https://datatracker.ietf.org/doc/html/rfc6761).

## Before the authorized deployment

1. Record AWS account, region, two AZs, operator/alternate, 24-hour stop time, budget alert, billing owner, and final-snapshot retention decision. Obtain account-specific estimates for the full architecture and the added EC2 instance, 8 GiB gp3 root volume, and **three single-AZ interface endpoint-hours plus data**. The two NAT gateways remain billable even though the access host uses PrivateLink. Confirm Temporal Cloud plan and billing separately.
2. Prepare the encrypted, versioned S3 backend and approved deployment identity. Arrange distinct runtime/migration/admin database roles, four image digests, Temporal Cloud endpoint and API key, four startup secret values, merchant credential broker and admin invoker roles. Approve the demo operator role trust/MFA and its temporary `ssm:StartSession` attachment. Confirm access to the chosen AL2023 AMI and SSM endpoints in the region.
3. **Only within the approved deployment window**, generate the actual short-lived certificate on a protected Windows machine with OpenSSL 3. Use a private ignored directory. Local preparation tested these options with a disposable certificate and deleted that test key; the deployment certificate has not been generated:

   ```powershell
   New-Item -ItemType Directory -Force .demo-certs | Out-Null
   openssl req -x509 -newkey rsa:2048 -noenc -sha256 -days 3 `
     -keyout .demo-certs/payflow-demo.key -out .demo-certs/payflow-demo.crt `
     -subj '/CN=payflow-demo.test' `
     -addext 'subjectAltName=DNS:payflow-demo.test' `
     -addext 'basicConstraints=critical,CA:FALSE' `
     -addext 'keyUsage=critical,digitalSignature,keyEncipherment' `
     -addext 'extendedKeyUsage=serverAuth'
   openssl x509 -in .demo-certs/payflow-demo.crt -noout -checkhost payflow-demo.test
   openssl verify -CAfile .demo-certs/payflow-demo.crt .demo-certs/payflow-demo.crt
   ```

   Inspect validity, SAN, and fingerprint locally. Restrict NTFS permissions on the key to the operator. ACM needs the PEM private key unencrypted at import, so keep it protected and delete the local key after the demo and certificate removal under the approved retention policy. Do not use AWS Private CA.
4. After separate approval, import the leaf certificate and key into ACM in the ALB region; capture **only** the returned ARN and certificate fingerprint. Do not run this during local preparation:

   ```powershell
   $region = 'us-east-1' # approved region
   aws acm import-certificate --region $region `
     --certificate fileb://.demo-certs/payflow-demo.crt `
     --private-key fileb://.demo-certs/payflow-demo.key
   ```

5. Copy `infrastructure/terraform/terraform.tfvars.example` to ignored `terraform.tfvars`. Set the imported ACM ARN, approved demo operator role, region/AZs, and `enable_demo_access=true`. Keep `private_client_cidr` absent unless a separate private route exists. Review `terraform plan` and cost estimate. Follow the staged infrastructure, secret, migration, backfill, admin, and scale gates in [deployment.md](deployment.md). Terraform does **not** create or store certificate material. Do not set the readiness attestations merely because a plan succeeded.

## Private PowerShell smoke test after deployment

Use a fresh synthetic merchant provisioned through the private one-off admin task and delivered by the approved credential broker. Do not put the bearer key in CLI arguments, environment variables, shell history, screenshots, or evidence. Install AWS CLI v2, Session Manager plugin, Go 1.27.2, and OpenSSL 3. Run `New-Item -ItemType Directory -Force .tools | Out-Null`, then build the local test helper with `go build -o .tools/demo-client.exe ./cmd/demo-client`. It has an allowlist of smoke-test API paths, dials only `127.0.0.1`, rejects redirects and non-2xx responses, and uses Go's certificate-chain and hostname verification. Use the exact private ALB DNS and instance ID from Terraform outputs. In the first PowerShell window:

```powershell
$region = 'us-east-1' # chosen deployed region
$instance = terraform -chdir=infrastructure/terraform output -raw demo_access_instance_id
$albDns = terraform -chdir=infrastructure/terraform output -raw private_alb_dns_name
$node = aws ssm describe-instance-information --region $region --output json |
  ConvertFrom-Json | Select-Object -ExpandProperty InstanceInformationList |
  Where-Object InstanceId -eq $instance
if (-not $node -or $node.PingStatus -ne 'Online' -or
    [version]$node.AgentVersion -lt [version]'3.3.4851.0') {
  throw 'SSM access host is offline or its agent is below the patched version.'
}
$node | Select-Object InstanceId, PingStatus, AgentVersion
aws ssm start-session --region $region --target $instance `
  --document-name AWS-StartPortForwardingSessionToRemoteHost `
  --parameters "host=$albDns,portNumber=443,localPortNumber=18443"
```

Confirm the session prints that local port `18443` is open and leave the window running. In a second PowerShell window, verify TLS and readiness. The client sends the request to `payflow-demo.test` while connecting to the loopback tunnel:

```powershell
'{"path":"/readyz"}' | .\.tools\demo-client.exe `
  -cert '.demo-certs/payflow-demo.crt' -local-port 18443
```

Require the helper's `verified TLS for payflow-demo.test; HTTP 200` line. An expired certificate, a wrong SAN, a different certificate, or a closed tunnel must fail; do not bypass TLS verification. Then run the bounded smoke procedure:

```powershell
.\scripts\demo-smoke.ps1 -CertificatePath '.demo-certs/payflow-demo.crt' `
  -MerchantId 'fresh-synthetic-demo-merchant'
```

The script prompts for the bearer key as a secure input and supplies it to the Go client over stdin, not process arguments. It creates at most 20 **synthetic** JPY 100 payments, selects one ID for each server-side 80/20 mock outcome policy, submits only those two, polls `GetPayment` up to two minutes, and checks that approval is `COMPLETED`, decline is `FAILED`, and the fresh merchant's balance is JPY 100. Extra created rows stay `CREATED` and unsubmitted. `results/aws-demo/smoke-redacted.json` is ignored by Git; it contains IDs, statuses, balance, and cert hash, never the key. A partial status or timeout is **incomplete**, not a failure verdict for the payment. The script's policy prediction is a selector; observed terminal statuses are authoritative. Reuse the submitted IDs to verify repeated `SubmitPayment` and `GetPayment` return the same logical payment, then inspect database-backed ledger evidence via the private admin path.

For **financial verification**, invoke the existing private admin task with `inspect PAYMENT_UUID` for each submitted payment and `issues` (syntax in [deployment.md](deployment.md)). Use the approved admin invoker role, private application subnets, `admin_security_group`, and `assignPublicIp=DISABLED`; pass only the payment ID in the task override. Wait for each one-off task to stop and check its exit code and redacted JSON log. Verify one confirmed mock provider operation and one balanced two-entry approval journal for the completed payment; verify **no** approval journal for the declined payment; verify merchant balance equals the approved amount and `issues` reports no relevant ledger violation. The API balance check alone is insufficient to claim ledger correctness. Do not pass a credential to the admin task. If any task, workflow, or provider result is uncertain, record it as pending investigation.

The following PowerShell pattern invokes one inspection; repeat it for the declined ID and `issues` (which takes no payment ID). It creates only ignored local JSON argument files. The invoker needs approved `ecs:RunTask`, `iam:PassRole`, `ecs:DescribeTasks`, and CloudWatch read access; the task's own role remains limited as described in [AWS architecture](aws-architecture.md):

```powershell
$region = 'us-east-1' # chosen deployed region
$evidence = Get-Content results/aws-demo/smoke-redacted.json | ConvertFrom-Json
$network = terraform -chdir=infrastructure/terraform output -json network_ids | ConvertFrom-Json
$adminTask = terraform -chdir=infrastructure/terraform output -raw admin_task_definition_arn
$net = @{ awsvpcConfiguration = @{
    subnets = @($network.app_subnets); securityGroups = @($network.admin_security_group)
    assignPublicIp = 'DISABLED'
} } | ConvertTo-Json -Compress -Depth 5
$override = @{ containerOverrides = @(@{ name = 'admin'; command = @('inspect', $evidence.approvedPaymentId) }) } |
  ConvertTo-Json -Compress -Depth 5
$net | Set-Content results/aws-demo/admin-network.json -Encoding ascii
$override | Set-Content results/aws-demo/admin-inspect.json -Encoding ascii
$taskArn = aws ecs run-task --region $region --cluster $network.cluster `
  --task-definition $adminTask --launch-type FARGATE `
  --network-configuration file://results/aws-demo/admin-network.json `
  --overrides file://results/aws-demo/admin-inspect.json `
  --query 'tasks[0].taskArn' --output text
if (-not $taskArn -or $taskArn -eq 'None') { throw 'Admin task did not start; inspect run-task failures.' }
aws ecs wait tasks-stopped --region $region --cluster $network.cluster --tasks $taskArn
$task = aws ecs describe-tasks --region $region --cluster $network.cluster --tasks $taskArn --output json | ConvertFrom-Json
if ($task.tasks[0].containers[0].exitCode -ne 0) { throw 'Admin inspect task failed.' }
$taskId = ($taskArn -split '/')[-1]
aws logs get-log-events --region $region --log-group-name '/ecs/payflow-demo/admin' `
  --log-stream-name "admin/admin/$taskId" --query 'events[].message' --output text
```

Check the admin log group name and stream prefix against the actual task definition before using the log command; do not publish raw logs. For the second inspection, replace the override command with `@('inspect', $evidence.declinedPaymentId)`; for the integrity scan use `@('issues')`. Recreate the JSON override and run a **new** one-off task each time. Check every exit code and save only reviewed, redacted output. `issues` may find unrelated historical records, which need separate review instead of being silently ignored.

## Evidence and screenshots

Save screenshots under ignored `results/aws-demo/`; redact account IDs if shared, merchant bearer tokens, RDS/Temporal URLs, secret values, private key paths/content, and any raw request headers. Capture the UTC timestamp and resource identifier separately in the deployment ticket. Required views:

1. ECS cluster services: API and worker desired/running counts plus healthy API target and worker readiness, with environment shown.
2. RDS PostgreSQL: `Available`, Multi-AZ, encrypted, private access, backup and deletion-protection settings; hide endpoints and credentials.
3. TLS/API: SSM session target and the Go client's verified TLS/HTTP 200 line for `/readyz`, plus authenticated Create/Get/Submit responses with bearer header hidden.
4. Payment results: the two synthetic payment IDs with `COMPLETED` and `FAILED` after polling; no screenshot of a mere successful HTTP submit as proof of settlement.
5. Temporal Cloud: workflow ID `payflow-payment-<payment ID>`, execution status/history, and the matching submitted payment ID; hide API keys and namespace secrets.
6. CloudWatch: sanitized API and worker application logs for the same payment/workflow IDs, including durable dispatch and outcome; hide connection strings and unrelated tenant data.
7. Financial ledger: redacted admin `inspect` output showing one balanced approval journal and no declined journal, `issues` output, and merchant balance JPY 100. Record any incomplete check as such.

Keep the redacted JSON evidence and approved screenshots only under the agreed retention policy. SSM tunnel contents themselves are not recorded in Session Manager logs. Stop the demo at the recorded deadline and use [teardown.md](teardown.md); verify actual residual resources and charges after teardown.
