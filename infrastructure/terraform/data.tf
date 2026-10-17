data "aws_caller_identity" "current" {}

data "aws_partition" "current" {}

resource "aws_db_subnet_group" "main" {
  name       = "${local.name}-db"
  subnet_ids = aws_subnet.database[*].id
}

resource "aws_db_parameter_group" "main" {
  name   = "${local.name}-postgres17"
  family = "postgres17"
  parameter {
    name  = "rds.force_ssl"
    value = "1"
  }
}

resource "aws_db_instance" "main" {
  identifier                   = "${local.name}-postgres"
  engine                       = "postgres"
  engine_version               = "17.6"
  instance_class               = var.db_instance_class
  allocated_storage            = 20
  max_allocated_storage        = 100
  storage_type                 = "gp3"
  storage_encrypted            = true
  db_name                      = "payflow"
  username                     = "payflow_admin"
  manage_master_user_password  = true
  multi_az                     = true
  publicly_accessible          = false
  db_subnet_group_name         = aws_db_subnet_group.main.name
  parameter_group_name         = aws_db_parameter_group.main.name
  vpc_security_group_ids       = [aws_security_group.database.id]
  backup_retention_period      = 7
  deletion_protection          = true
  skip_final_snapshot          = false
  final_snapshot_identifier    = "${local.name}-final"
  auto_minor_version_upgrade   = true
  performance_insights_enabled = false
}

# Terraform manages secret containers only. Secret values must be supplied
# outside Terraform, so credentials never enter state or plan output.
resource "aws_secretsmanager_secret" "database_runtime_url" {
  name                    = "${local.name}/database-runtime-url"
  recovery_window_in_days = 30
}

resource "aws_secretsmanager_secret" "database_migration_url" {
  name                    = "${local.name}/database-migration-url"
  recovery_window_in_days = 30
}

resource "aws_secretsmanager_secret" "database_admin_url" {
  name                    = "${local.name}/database-admin-url"
  recovery_window_in_days = 30
}

resource "aws_secretsmanager_secret" "temporal_api_key" {
  name                    = "${local.name}/temporal-api-key"
  recovery_window_in_days = 30
}

resource "aws_ecr_repository" "api" {
  name                 = "${local.name}/api"
  image_tag_mutability = "IMMUTABLE"
  image_scanning_configuration { scan_on_push = true }
  encryption_configuration { encryption_type = "AES256" }
}

resource "aws_ecr_repository" "worker" {
  name                 = "${local.name}/worker"
  image_tag_mutability = "IMMUTABLE"
  image_scanning_configuration { scan_on_push = true }
  encryption_configuration { encryption_type = "AES256" }
}

resource "aws_ecr_repository" "migrate" {
  name                 = "${local.name}/migrate"
  image_tag_mutability = "IMMUTABLE"
  image_scanning_configuration { scan_on_push = true }
  encryption_configuration { encryption_type = "AES256" }
}

resource "aws_ecr_repository" "admin" {
  name                 = "${local.name}/admin"
  image_tag_mutability = "IMMUTABLE"
  image_scanning_configuration { scan_on_push = true }
  encryption_configuration { encryption_type = "AES256" }
}

resource "aws_cloudwatch_log_group" "api" {
  name              = "/ecs/${local.name}/api"
  retention_in_days = var.log_retention_days
}

resource "aws_cloudwatch_log_group" "worker" {
  name              = "/ecs/${local.name}/worker"
  retention_in_days = var.log_retention_days
}

resource "aws_cloudwatch_log_group" "migrate" {
  name              = "/ecs/${local.name}/migrate"
  retention_in_days = var.log_retention_days
}

resource "aws_cloudwatch_log_group" "admin" {
  name              = "/ecs/${local.name}/admin"
  retention_in_days = var.log_retention_days
}

resource "aws_lb" "internal" {
  name                       = "${local.name}-alb"
  internal                   = true
  load_balancer_type         = "application"
  security_groups            = [aws_security_group.alb.id]
  subnets                    = aws_subnet.app[*].id
  drop_invalid_header_fields = true
}

resource "aws_lb_target_group" "api" {
  name        = "${local.name}-api"
  port        = 8080
  protocol    = "HTTP"
  target_type = "ip"
  vpc_id      = aws_vpc.main.id
  health_check {
    path                = "/readyz"
    protocol            = "HTTP"
    healthy_threshold   = 2
    unhealthy_threshold = 3
    interval            = 30
  }
}

resource "aws_lb_listener" "https" {
  load_balancer_arn = aws_lb.internal.arn
  port              = 443
  protocol          = "HTTPS"
  ssl_policy        = "ELBSecurityPolicy-TLS13-1-2-2021-06"
  certificate_arn   = var.internal_alb_certificate_arn
  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.api.arn
  }
}

output "private_alb_dns_name" {
  value       = aws_lb.internal.dns_name
  description = "Private VPC DNS name; requires an approved private network path and a matching certificate hostname."
}

output "database_address" {
  value = aws_db_instance.main.address
}

output "secret_arns" {
  value = { database_runtime_url = aws_secretsmanager_secret.database_runtime_url.arn, database_migration_url = aws_secretsmanager_secret.database_migration_url.arn, database_admin_url = aws_secretsmanager_secret.database_admin_url.arn, temporal_api_key = aws_secretsmanager_secret.temporal_api_key.arn }
}

output "ecr_repository_urls" {
  value = { api = aws_ecr_repository.api.repository_url, worker = aws_ecr_repository.worker.repository_url, migrate = aws_ecr_repository.migrate.repository_url, admin = aws_ecr_repository.admin.repository_url }
}

output "network_ids" {
  value = { cluster = aws_ecs_cluster.main.arn, app_subnets = aws_subnet.app[*].id, migration_security_group = aws_security_group.migrate.id, admin_security_group = aws_security_group.admin.id }
}
