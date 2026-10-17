locals {
  merchant_secret_prefix = "${local.name}/merchant-api-key"
  merchant_secret_arn    = "arn:${data.aws_partition.current.partition}:secretsmanager:${var.aws_region}:${data.aws_caller_identity.current.account_id}:secret:${local.merchant_secret_prefix}/*"
}

# Only the one-off admin task can create merchant key secrets, and it cannot
# retrieve their values. No admin task is exposed as an ECS service or HTTP API.
resource "aws_iam_role_policy" "admin_secret_delivery" {
  name = "create-merchant-key-secrets"
  role = aws_iam_role.task["admin"].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["secretsmanager:CreateSecret"]
      Resource = local.merchant_secret_arn
    }]
  })
}

# The credential broker is an existing, separately approved role. The
# merchant-key value is never delivered to the ECS invoker or task logs.
resource "aws_iam_role_policy" "credential_recipient" {
  count = var.enable_admin ? 1 : 0
  name  = "${local.name}-merchant-key-read"
  role  = var.credential_recipient_role_name
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"]
      Resource = local.merchant_secret_arn
    }]
  })
}

resource "aws_ecs_task_definition" "admin" {
  count                    = var.enable_admin ? 1 : 0
  family                   = "${local.name}-admin"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = "256"
  memory                   = "512"
  execution_role_arn       = aws_iam_role.execution["admin"].arn
  task_role_arn            = aws_iam_role.task["admin"].arn
  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "X86_64"
  }
  container_definitions = jsonencode([{
    name      = "admin"
    image     = "${aws_ecr_repository.admin.repository_url}@${var.admin_image_digest}"
    essential = true
    environment = [
      { name = "PAYFLOW_ENV", value = "production" },
      { name = "PAYFLOW_ADMIN_ACTOR", value = "private-ecs-admin" },
      { name = "PAYFLOW_CREDENTIAL_SECRET_PREFIX", value = local.merchant_secret_prefix }
    ]
    secrets          = [{ name = "DATABASE_URL", valueFrom = aws_secretsmanager_secret.database_admin_url.arn }]
    logConfiguration = { logDriver = "awslogs", options = { "awslogs-group" = aws_cloudwatch_log_group.admin.name, "awslogs-region" = var.aws_region, "awslogs-stream-prefix" = "admin" } }
  }])
  depends_on = [aws_iam_role_policy.execution, aws_iam_role_policy.admin_secret_delivery]
}

# Attach invocation rights only to the named pre-existing administrator role.
# Its own trust policy, MFA, and approval flow remain with the account owner.
resource "aws_iam_role_policy" "admin_invoker" {
  count = var.enable_admin ? 1 : 0
  name  = "${local.name}-invoke-private-admin"
  role  = var.admin_invoker_role_name
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect    = "Allow"
        Action    = ["ecs:RunTask"]
        Resource  = aws_ecs_task_definition.admin[0].arn
        Condition = { ArnEquals = { "ecs:cluster" = aws_ecs_cluster.main.arn } }
      },
      {
        Effect    = "Allow"
        Action    = ["iam:PassRole"]
        Resource  = [aws_iam_role.execution["admin"].arn, aws_iam_role.task["admin"].arn]
        Condition = { StringEquals = { "iam:PassedToService" = "ecs-tasks.amazonaws.com" } }
      },
      {
        Effect   = "Allow"
        Action   = ["ecs:DescribeTasks"]
        Resource = "arn:${data.aws_partition.current.partition}:ecs:${var.aws_region}:${data.aws_caller_identity.current.account_id}:task/${aws_ecs_cluster.main.name}/*"
      }
    ]
  })
}

output "admin_task_definition_arn" {
  value = var.enable_admin ? aws_ecs_task_definition.admin[0].arn : null
}

output "merchant_key_secret_prefix" {
  value = local.merchant_secret_prefix
}
