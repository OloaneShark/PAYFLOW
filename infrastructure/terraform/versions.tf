terraform {
  required_version = ">= 1.14.0, < 2.0.0"
  backend "s3" {}
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "= 6.64.0"
    }
  }
}

provider "aws" {
  region = var.aws_region
  default_tags {
    tags = { Project = "PAYFLOW", Environment = var.environment, ManagedBy = "Terraform" }
  }
}
