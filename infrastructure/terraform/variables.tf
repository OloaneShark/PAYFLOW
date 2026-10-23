variable "aws_region" {
  type    = string
  default = "us-east-1"
}

variable "environment" {
  type    = string
  default = "demo"
  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,14}$", var.environment))
    error_message = "Use a short lowercase environment name."
  }
}

variable "availability_zones" {
  type    = list(string)
  default = ["us-east-1a", "us-east-1b"]
  validation {
    condition     = length(var.availability_zones) == 2 && length(distinct(var.availability_zones)) == 2
    error_message = "Supply two distinct availability zones in the selected region."
  }
}

variable "vpc_cidr" {
  type    = string
  default = "10.60.0.0/16"
}

variable "private_client_cidr" {
  type        = string
  default     = null
  description = "Optional existing routed private client CIDR. Leave null for SSM-only demo access."
  validation {
    condition     = var.private_client_cidr == null || (can(cidrhost(var.private_client_cidr, 0)) && var.private_client_cidr != "0.0.0.0/0" && var.private_client_cidr != "::/0")
    error_message = "Supply the specific VPN or private network CIDR allowed to reach the internal ALB."
  }
}

variable "enable_demo_access" {
  type        = bool
  default     = false
  description = "Create one temporary private SSM access host for the domain-free demonstration."
}

variable "demo_access_operator_role_name" {
  type        = string
  default     = ""
  description = "Existing approved IAM role allowed to open a remote-host port-forwarding session to the demo access host."
  validation {
    condition     = !var.enable_demo_access || var.demo_access_operator_role_name != ""
    error_message = "Supply an approved existing operator role when enabling demo access."
  }
}

variable "internal_alb_certificate_arn" {
  type        = string
  description = "Regional ACM ARN of the imported short-lived payflow-demo.test certificate. Import outside Terraform; never put the key in state."
  validation {
    condition     = can(regex("^arn:aws:acm:[^:]+:[0-9]{12}:certificate/", var.internal_alb_certificate_arn))
    error_message = "Supply an ACM certificate ARN for the internal HTTPS listener."
  }
}

variable "enable_runtime" {
  type        = bool
  default     = false
  description = "Create ECS task definitions and services after images, database credentials, and Temporal Cloud credentials are ready. Services still default to zero desired tasks until migration completes."
}

variable "migrations_complete" {
  type        = bool
  default     = false
  description = "Set true only after the one-off migration task has succeeded and historical backfill has been reviewed."
}

variable "deployment_prerequisites_reviewed" {
  type        = bool
  default     = false
  description = "Manual attestation that private routing, secrets, Temporal Cloud, merchant credentials, image digests, migrations, backfill, and health checks have been reviewed. Terraform cannot verify external systems."
}

variable "temporal_address" {
  type    = string
  default = ""
  validation {
    condition     = !var.enable_runtime || (can(regex("^[a-z0-9.-]+:7233$", var.temporal_address)) && !startswith(var.temporal_address, "127.") && !startswith(var.temporal_address, "localhost"))
    error_message = "Supply the external Temporal Cloud endpoint with port 7233."
  }
}

variable "temporal_namespace" {
  type    = string
  default = ""
  validation {
    condition     = !var.enable_runtime || (var.temporal_namespace != "" && var.temporal_namespace != "default")
    error_message = "Supply the provisioned Temporal Cloud namespace."
  }
}

variable "api_image_digest" {
  type    = string
  default = ""
  validation {
    condition     = !var.enable_runtime || can(regex("^sha256:[a-f0-9]{64}$", var.api_image_digest))
    error_message = "Pin the API image by SHA-256 digest."
  }
}

variable "worker_image_digest" {
  type    = string
  default = ""
  validation {
    condition     = !var.enable_runtime || can(regex("^sha256:[a-f0-9]{64}$", var.worker_image_digest))
    error_message = "Pin the worker image by SHA-256 digest."
  }
}

variable "migrate_image_digest" {
  type    = string
  default = ""
  validation {
    condition     = !var.enable_runtime || can(regex("^sha256:[a-f0-9]{64}$", var.migrate_image_digest))
    error_message = "Pin the migrator image by SHA-256 digest."
  }
}

variable "enable_admin" {
  type        = bool
  default     = false
  description = "Register the private one-off administrator task after its recipient and invoker roles are approved."
  validation {
    condition     = !var.enable_admin || var.enable_runtime
    error_message = "Administration requires runtime task definitions to be enabled."
  }
}

variable "admin_image_digest" {
  type    = string
  default = ""
  validation {
    condition     = !var.enable_admin || can(regex("^sha256:[a-f0-9]{64}$", var.admin_image_digest))
    error_message = "Pin the administrator image by SHA-256 digest."
  }
}

variable "credential_recipient_role_name" {
  type        = string
  default     = ""
  description = "Existing IAM role for the approved credential broker; Terraform grants it read access only to PAYFLOW merchant-key secrets."
  validation {
    condition     = !var.enable_admin || var.credential_recipient_role_name != ""
    error_message = "Supply an approved existing credential recipient role when enabling administration."
  }
}

variable "admin_invoker_role_name" {
  type        = string
  default     = ""
  description = "Existing IAM role for authorized administrators to invoke only the private admin task."
  validation {
    condition     = !var.enable_admin || var.admin_invoker_role_name != ""
    error_message = "Supply an approved existing admin invoker role when enabling administration."
  }
}

variable "api_desired_count" {
  type    = number
  default = 0
  validation {
    condition     = var.api_desired_count >= 0 && var.api_desired_count <= 10
    error_message = "API desired count must be 0..10."
  }
}

variable "worker_desired_count" {
  type    = number
  default = 0
  validation {
    condition     = var.worker_desired_count >= 0 && var.worker_desired_count <= 10
    error_message = "Worker desired count must be 0..10."
  }
}

variable "log_retention_days" {
  type    = number
  default = 30
  validation {
    condition     = contains([1, 3, 5, 7, 14, 30, 60, 90, 120, 150, 180, 365], var.log_retention_days)
    error_message = "Use a supported CloudWatch log retention period."
  }
}

variable "db_instance_class" {
  type    = string
  default = "db.t4g.small"
}
