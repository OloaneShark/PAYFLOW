# AWS cost notes

PAYFLOW's proposed AWS layout has recurring costs even when no API or worker task is running. Confirm current regional rates with the [AWS Pricing Calculator](https://calculator.aws/) before a deployment decision; this repository does not claim a fixed monthly price.

| Category | Cost driver |
| --- | --- |
| RDS PostgreSQL | Multi-AZ instance hours, provisioned gp3 storage, backups and retained final snapshots |
| NAT gateways | Two gateway hours and data processed, including image pulls and Temporal Cloud traffic |
| Internal ALB | ALB hours and load balancer capacity units |
| ECS Fargate | API, worker, one-off migration, and one-off private administration vCPU and memory duration; Container Insights adds telemetry charges |
| ECR | Stored image layers and any scanning or transfer charges |
| CloudWatch | Ingested JSON logs, retained bytes, metrics, and alarms |
| Secrets Manager | Runtime/migration/admin/Temporal startup secrets, one merchant-key secret per issue or rotation, and API calls; AWS-managed RDS master secret may have different treatment |
| Data transfer | Cross-zone, NAT, and external Temporal Cloud traffic |
| Temporal Cloud | Independently billed namespace and usage under the selected Temporal plan |
| Private connectivity and DNS | VPN/Direct Connect, private DNS, certificate, and remote state storage if supplied |

The two NAT gateways, Multi-AZ RDS, and ALB are the largest always-on categories in this small demonstration design. A zero desired task count does not shut them down. Use an explicit time-box and budget alerts for any temporary environment. Do not remove TLS, private routing, backups, or reconciliation safeguards merely to lower cost.

## Deployment cost checklist

- [ ] Select region, expected run duration, and API/worker desired counts.
- [ ] Estimate Multi-AZ RDS, storage, backup retention, and final snapshot lifetime.
- [ ] Estimate two NAT gateways, expected outbound data, and private client connectivity.
- [ ] Estimate ALB, Fargate CPU/memory, ECR storage, CloudWatch retention, and Secrets Manager including merchant-key growth and retirement.
- [ ] Include Temporal Cloud namespace and usage under its separate account.
- [ ] Set an account budget/alert and name the owner responsible for teardown and retained data.
