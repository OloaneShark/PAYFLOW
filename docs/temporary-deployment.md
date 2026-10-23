# Temporary AWS demonstration plan

This is a planning worksheet for a **separately authorized** 24-hour PAYFLOW demonstration, with an optional 48-hour extension. PAYFLOW moves no real money. Phase 9 creates no AWS or Temporal Cloud resources. Record the owner, budget, exact timestamps, and approved estimate before any deployment.

| Field | Record before deployment |
| --- | --- |
| AWS account and region | Pending |
| Deployment owner and alternate | Pending |
| Cost/billing owner | Pending |
| Teardown owner and alternate | Pending |
| Approved start, UTC | Pending |
| Planned stop, UTC (start + 24 hours) | Pending |
| Optional extension approval and stop, UTC (no later than start + 48 hours) | Pending |
| Pricing Calculator estimate, assumptions, and budget threshold | Pending |
| Temporal Cloud billing and cleanup owner | Pending |

Do not start the clock at `terraform apply` completion: RDS, NAT gateways, ALB, ECR, Secrets Manager, logging, and state can begin billing during infrastructure creation. Record the actual first billable resource timestamp and update the stop deadline. A zero ECS desired count still leaves infrastructure charges. Request an extension before the 24-hour deadline; otherwise begin teardown early enough to finish by that deadline.

## Billable inventory and estimate inputs

Use the [AWS Pricing Calculator](https://calculator.aws/) for the chosen account and region. Enter **two** NAT gateways and Elastic IP allocations, a **Multi-AZ** PostgreSQL 17 RDS instance (`db.t4g.small` by default) with 20 GiB gp3 and possible autoscaling, seven-day backup retention plus a final snapshot, one internal ALB and its capacity usage, Fargate API and worker task CPU/memory hours plus migration/admin task runs, **one private `t3.micro` SSM access host with 8 GiB gp3 and three single-AZ interface endpoints**, four ECR repositories and stored image layers, CloudWatch log ingestion/retention/alarms and Container Insights metrics, Secrets Manager startup secrets and per-merchant key secrets, state bucket requests/storage, cross-zone and internet egress. The imported self-signed ACM certificate and reserved `.test` hostname avoid a purchased domain and AWS Private CA; check current ACM charges for the chosen service. Temporal Cloud is billed separately; obtain its namespace and usage estimate from its owner. Use expected 24-hour and 48-hour hours in separate estimate scenarios. Record actual data transfer and storage assumptions rather than assuming zero usage.

The project intentionally keeps private networking, TLS, backups, RDS deletion protection, and distinct IAM/database roles. Review the module's actual plan against this inventory and the [AWS architecture](aws-architecture.md). Rates, taxes, credits, and usage vary; this document makes no fixed-price claim. The estimate remains **NOT CONFIGURED** until an owner records calculator output and a budget threshold.

| Baseline for a 24-hour window | Resource quantity to price |
| --- | --- |
| NAT and load balancing | 2 NAT gateways (48 gateway-hours), 2 Elastic IP allocations, 1 internal ALB (24 ALB-hours plus LCU usage) |
| Database | 1 Multi-AZ RDS PostgreSQL deployment for 24 hours, 20 GiB gp3 minimum, 7-day automated backup policy and an explicitly retained final snapshot |
| Running services | 1 API task at 0.25 vCPU/0.5 GiB and 1 worker task at 0.5 vCPU/1 GiB for the actual scaled hours; one-off migration and admin tasks billed for their run durations |
| Temporary private access | 1 `t3.micro` for at most 24 instance-hours, 8 GiB gp3 root volume, 3 single-AZ interface endpoints (72 endpoint-hours plus processing), no public IP |
| Usage and retained resources | 4 ECR repositories/images, 4 startup secrets plus each issued merchant key, CloudWatch logs/alarms/Container Insights, data transfer, state bucket, Temporal Cloud usage, backups and any retained snapshot |

The 48-hour extension doubles the fixed hourly quantities while preserving the separately measured storage, traffic, task, and Temporal usage assumptions. Confirm the actual account-specific USD estimate with the [AWS Pricing Calculator](https://calculator.aws/) and record it above; this table is a quantity estimate, not a price quote.

## Run window

Before the authorized start, complete [local preflight](deployment-readiness.md), externally verify the deployment gate, approve a capped budget, and schedule teardown ownership. During the demo, record the actual infrastructure start timestamp, API/worker scaling timestamp, observed health and ledger checks, and the planned stop. Monitor pending dispatch, expired claims, reconciliation exhaustion, ledger integrity, RDS alarms, worker availability, and API readiness. Preserve unknown provider outcomes for operator investigation.

At the stop deadline, cease traffic and follow [teardown.md](teardown.md). Do not treat stopping API and worker tasks as cost shutdown. Verify AWS billing after teardown and again after delayed usage posts; retained snapshots, Secrets Manager recovery windows, ECR layers, logs, state, and Temporal Cloud can continue to accrue charges.
