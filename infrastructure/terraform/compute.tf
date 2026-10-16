resource "aws_ecs_cluster" "main" {
  name = local.name
  setting {
    name  = "containerInsights"
    value = "enabled"
  }
}

locals {
  task_names = toset(["api", "worker", "migrate"])
  repository_arns = {
    api     = aws_ecr_repository.api.arn
    worker  = aws_ecr_repository.worker.arn
    migrate = aws_ecr_repository.migrate.arn
  }
  log_group_arns = {
    api     = aws_cloudwatch_log_group.api.arn
    worker  = aws_cloudwatch_log_group.worker.arn
    migrate = aws_cloudwatch_log_group.migrate.arn
  }
  task_secrets = {
    api     = [aws_secretsmanager_secret.database_runtime_url.arn]
    worker  = [aws_secretsmanager_secret.database_runtime_url.arn, aws_secretsmanager_secret.temporal_api_key.arn]
    migrate = [aws_secretsmanager_secret.database_migration_url.arn]
  }
}

data "aws_iam_policy_document" "ecs_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "execution" {
  for_each           = local.task_names
  name               = "${local.name}-${each.key}-execution"
  assume_role_policy = data.aws_iam_policy_document.ecs_assume.json
}

resource "aws_iam_role_policy" "execution" {
  for_each = local.task_names
  name     = "bootstrap"
  role     = aws_iam_role.execution[each.key].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      { Effect = "Allow", Action = ["ecr:GetAuthorizationToken"], Resource = "*" },
      { Effect = "Allow", Action = ["ecr:BatchCheckLayerAvailability", "ecr:GetDownloadUrlForLayer", "ecr:BatchGetImage"], Resource = local.repository_arns[each.key] },
      { Effect = "Allow", Action = ["logs:CreateLogStream", "logs:PutLogEvents"], Resource = "${trimsuffix(local.log_group_arns[each.key], ":*")}:*" },
      { Effect = "Allow", Action = ["secretsmanager:GetSecretValue"], Resource = local.task_secrets[each.key] }
    ]
  })
}

# Application roles have no AWS permissions. The execution role alone pulls
# images, opens log streams and injects startup secrets.
resource "aws_iam_role" "task" {
  for_each           = local.task_names
  name               = "${local.name}-${each.key}-task"
  assume_role_policy = data.aws_iam_policy_document.ecs_assume.json
}

resource "aws_ecs_task_definition" "api" {
  count                    = var.enable_runtime ? 1 : 0
  family                   = "${local.name}-api"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = "256"
  memory                   = "512"
  execution_role_arn       = aws_iam_role.execution["api"].arn
  task_role_arn            = aws_iam_role.task["api"].arn
  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "X86_64"
  }
  container_definitions = jsonencode([{
    name         = "api"
    image        = "${aws_ecr_repository.api.repository_url}@${var.api_image_digest}"
    essential    = true
    portMappings = [{ containerPort = 8080, hostPort = 8080, protocol = "tcp" }]
    environment = [
      { name = "PAYFLOW_ENV", value = "production" },
      { name = "LISTEN_ADDR", value = "0.0.0.0:8080" },
      { name = "PAYFLOW_ENABLE_DEV_PROCESSING", value = "false" }
    ]
    secrets          = [{ name = "DATABASE_URL", valueFrom = aws_secretsmanager_secret.database_runtime_url.arn }]
    logConfiguration = { logDriver = "awslogs", options = { "awslogs-group" = aws_cloudwatch_log_group.api.name, "awslogs-region" = var.aws_region, "awslogs-stream-prefix" = "api" } }
    healthCheck      = { command = ["CMD", "/app/healthcheck", "http://127.0.0.1:8080/readyz"], interval = 30, timeout = 3, retries = 3, startPeriod = 20 }
  }])
  depends_on = [aws_iam_role_policy.execution]
}

resource "aws_ecs_task_definition" "worker" {
  count                    = var.enable_runtime ? 1 : 0
  family                   = "${local.name}-worker"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = "512"
  memory                   = "1024"
  execution_role_arn       = aws_iam_role.execution["worker"].arn
  task_role_arn            = aws_iam_role.task["worker"].arn
  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "X86_64"
  }
  container_definitions = jsonencode([{
    name      = "worker"
    image     = "${aws_ecr_repository.worker.repository_url}@${var.worker_image_digest}"
    essential = true
    environment = [
      { name = "PAYFLOW_ENV", value = "production" },
      { name = "TEMPORAL_ADDRESS", value = var.temporal_address },
      { name = "TEMPORAL_NAMESPACE", value = var.temporal_namespace },
      { name = "PAYFLOW_WORKER_METRICS_ADDR", value = "127.0.0.1:9091" }
    ]
    secrets = [
      { name = "DATABASE_URL", valueFrom = aws_secretsmanager_secret.database_runtime_url.arn },
      { name = "TEMPORAL_API_KEY", valueFrom = aws_secretsmanager_secret.temporal_api_key.arn }
    ]
    logConfiguration = { logDriver = "awslogs", options = { "awslogs-group" = aws_cloudwatch_log_group.worker.name, "awslogs-region" = var.aws_region, "awslogs-stream-prefix" = "worker" } }
    healthCheck      = { command = ["CMD", "/app/healthcheck", "http://127.0.0.1:9091/readyz"], interval = 30, timeout = 3, retries = 3, startPeriod = 45 }
  }])
  depends_on = [aws_iam_role_policy.execution]
}

resource "aws_ecs_task_definition" "migrate" {
  count                    = var.enable_runtime ? 1 : 0
  family                   = "${local.name}-migrate"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = "256"
  memory                   = "512"
  execution_role_arn       = aws_iam_role.execution["migrate"].arn
  task_role_arn            = aws_iam_role.task["migrate"].arn
  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "X86_64"
  }
  container_definitions = jsonencode([{
    name      = "migrate"
    image     = "${aws_ecr_repository.migrate.repository_url}@${var.migrate_image_digest}"
    essential = true
    secrets   = [{ name = "DATABASE_URL", valueFrom = aws_secretsmanager_secret.database_migration_url.arn }]
    environment = [
      { name = "PAYFLOW_ENV", value = "production" },
      { name = "PAYFLOW_MIGRATION_TIMEOUT", value = "10m" }
    ]
    logConfiguration = { logDriver = "awslogs", options = { "awslogs-group" = aws_cloudwatch_log_group.migrate.name, "awslogs-region" = var.aws_region, "awslogs-stream-prefix" = "migrate" } }
  }])
  depends_on = [aws_iam_role_policy.execution]
}

resource "aws_ecs_service" "api" {
  count           = var.enable_runtime ? 1 : 0
  name            = "api"
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.api[0].arn
  desired_count   = var.api_desired_count
  launch_type     = "FARGATE"
  network_configuration {
    subnets          = aws_subnet.app[*].id
    security_groups  = [aws_security_group.api.id]
    assign_public_ip = false
  }
  load_balancer {
    target_group_arn = aws_lb_target_group.api.arn
    container_name   = "api"
    container_port   = 8080
  }
  depends_on = [aws_lb_listener.https]
  lifecycle {
    precondition {
      condition     = var.api_desired_count == 0 || var.migrations_complete
      error_message = "Run the one-off migration and review any required backfill before scaling the API."
    }
  }
}

resource "aws_ecs_service" "worker" {
  count           = var.enable_runtime ? 1 : 0
  name            = "worker"
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.worker[0].arn
  desired_count   = var.worker_desired_count
  launch_type     = "FARGATE"
  network_configuration {
    subnets          = aws_subnet.app[*].id
    security_groups  = [aws_security_group.worker.id]
    assign_public_ip = false
  }
  lifecycle {
    precondition {
      condition     = var.worker_desired_count == 0 || var.migrations_complete
      error_message = "Run the one-off migration and review any required backfill before scaling the worker."
    }
  }
}

output "migration_task_definition_arn" {
  value = var.enable_runtime ? aws_ecs_task_definition.migrate[0].arn : null
}
