# main.tf

terraform {
  required_version = ">= 1.5"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

provider "aws" {
  region = var.aws_region
  default_tags { tags = local.common_tags }
}

# Fallback AMI lookup — only used when var.ami_id is empty (no Packer AMI).
# When using Packer, set ami_id to the built AMI and this is ignored.
data "aws_ami" "al2023_arm64" {
  count       = var.ami_id == "" ? 1 : 0
  most_recent = true
  owners      = ["amazon"]

  filter {
    name   = "name"
    values = ["al2023-ami-*-arm64"]
  }

  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }
}
