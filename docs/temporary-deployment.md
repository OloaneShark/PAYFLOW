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

Use the [AWS Pricing Calculator](https://calculator.aws/) for the chosen account and region. Enter **two** NAT gateways and Elastic IP allocations, a **Multi-AZ** PostgreSQL 17 RDS instance (`db.t4g.small` by default) with 20 GiB gp3 and possible autoscaling, seven-day backup retention plus a final snapshot, one internal ALB and its capacity usage, Fargate API and worker task CPU/memory hours plus migration/admin task runs, four ECR repositories and stored image layers, CloudWatch log ingestion/retention/alarms and Container Insights metrics, Secrets Manager startup secrets and per-merchant key secrets, state bucket requests/storage, cross-zone and internet egress, and private connectivity/DNS/certificate dependencies. Temporal Cloud is billed separately; obtain its namespace and usage estimate from its owner. Use expected 24-hour and 48-hour hours in separate estimate scenarios. Record actual data transfer and storage assumptions rather than assuming zero usage.

The project intentionally keeps private networking, TLS, backups, RDS deletion protection, and distinct IAM/database roles. Review the module's actual plan against this inventory and the [AWS architecture](aws-architecture.md). Rates, taxes, credits, and usage vary; this document makes no fixed-price claim. The estimate remains **NOT CONFIGURED** until an owner records calculator output and a budget threshold.

## Run window

Before the authorized start, complete [local preflight](deployment-readiness.md), externally verify the deployment gate, approve a capped budget, and schedule teardown ownership. During the demo, record the actual infrastructure start timestamp, API/worker scaling timestamp, observed health and ledger checks, and the planned stop. Monitor pending dispatch, expired claims, reconciliation exhaustion, ledger integrity, RDS alarms, worker availability, and API readiness. Preserve unknown provider outcomes for operator investigation.

At the stop deadline, cease traffic and follow [teardown.md](teardown.md). Do not treat stopping API and worker tasks as cost shutdown. Verify AWS billing after teardown and again after delayed usage posts; retained snapshots, Secrets Manager recovery windows, ECR layers, logs, state, and Temporal Cloud can continue to accrue charges.
