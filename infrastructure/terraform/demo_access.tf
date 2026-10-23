# The optional instance is a short-lived SSM tunnel endpoint, not an SSH bastion.
# It has no public IP, key pair, inbound rule, database access, or application role.
data "aws_ssm_parameter" "demo_access_ami" {
  count = var.enable_demo_access ? 1 : 0
  name  = "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64"
}

data "aws_iam_policy_document" "demo_access_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "demo_access" {
  count              = var.enable_demo_access ? 1 : 0
  name               = "${local.name}-demo-access"
  assume_role_policy = data.aws_iam_policy_document.demo_access_assume.json
}

resource "aws_iam_role_policy_attachment" "demo_access_ssm" {
  count      = var.enable_demo_access ? 1 : 0
  role       = aws_iam_role.demo_access[0].name
  policy_arn = "arn:${data.aws_partition.current.partition}:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

resource "aws_iam_instance_profile" "demo_access" {
  count = var.enable_demo_access ? 1 : 0
  name  = "${local.name}-demo-access"
  role  = aws_iam_role.demo_access[0].name
}

resource "aws_instance" "demo_access" {
  count                       = var.enable_demo_access ? 1 : 0
  ami                         = data.aws_ssm_parameter.demo_access_ami[0].value
  instance_type               = "t3.micro"
  subnet_id                   = aws_subnet.app[0].id
  vpc_security_group_ids      = [aws_security_group.demo_access[0].id]
  associate_public_ip_address = false
  iam_instance_profile        = aws_iam_instance_profile.demo_access[0].name
  monitoring                  = false
  metadata_options {
    http_endpoint = "enabled"
    http_tokens   = "required"
  }
  root_block_device {
    encrypted             = true
    volume_type           = "gp3"
    volume_size           = 8
    delete_on_termination = true
  }
  tags       = { Name = "${local.name}-demo-access", Purpose = "temporary-ssm-tunnel" }
  depends_on = [aws_iam_role_policy_attachment.demo_access_ssm]
}

# Attach session permission to an approved pre-existing role; its trust and MFA
# policy remain outside this module. This grants no shell or SSH document.
resource "aws_iam_role_policy" "demo_access_operator" {
  count = var.enable_demo_access ? 1 : 0
  name  = "${local.name}-demo-port-forward"
  role  = var.demo_access_operator_role_name
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect    = "Allow"
        Action    = ["ssm:StartSession"]
        Resource  = [aws_instance.demo_access[0].arn, "arn:${data.aws_partition.current.partition}:ssm:${var.aws_region}::document/AWS-StartPortForwardingSessionToRemoteHost"]
        Condition = { BoolIfExists = { "ssm:SessionDocumentAccessCheck" = "true" } }
      },
      {
        Effect   = "Allow"
        Action   = ["ssm:TerminateSession", "ssm:ResumeSession"]
        Resource = "arn:${data.aws_partition.current.partition}:ssm:${var.aws_region}:${data.aws_caller_identity.current.account_id}:session/$${aws:userid}-*"
      },
      {
        Effect   = "Allow"
        Action   = ["ssmmessages:OpenDataChannel"]
        Resource = "arn:${data.aws_partition.current.partition}:ssm:${var.aws_region}:${data.aws_caller_identity.current.account_id}:session/$${aws:userid}-*"
      },
      {
        Effect   = "Allow"
        Action   = ["ssm:DescribeInstanceInformation"]
        Resource = "*"
      }
    ]
  })
}

output "demo_access_instance_id" {
  value       = var.enable_demo_access ? aws_instance.demo_access[0].id : null
  description = "Temporary private SSM target; null when demo access is disabled."
}
