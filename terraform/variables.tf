# variables.tf

variable "environment" {
  description = "Environment name (staging, production)"
  type        = string
  default     = "production"
}

variable "aws_region" {
  description = "AWS region"
  type        = string
  default     = "us-east-1"
}

variable "vpc_id" {
  description = "VPC ID"
  type        = string
}

variable "private_subnet_ids" {
  description = "Private subnets for EC2 and Redis"
  type        = list(string)
}

variable "public_subnet_ids" {
  description = "Public subnets for ALB"
  type        = list(string)
}

variable "origin_servers" {
  description = "Commscope packager origins (semicolon-separated, e.g. 'packager1.internal:80;packager2.internal:80')"
  type        = string
}

variable "ami_id" {
  description = "AMI ID built by Packer. If empty, falls back to latest Amazon Linux 2023 ARM64 (requires binary_s3_bucket)."
  type        = string
  default     = ""
}

variable "instance_type" {
  description = "EC2 instance type (ARM64 Graviton)"
  type        = string
  default     = "t4g.micro"
}

variable "desired_capacity" {
  description = "Desired number of EC2 instances"
  type        = number
  default     = 2
}

variable "min_size" {
  description = "Minimum ASG size"
  type        = number
  default     = 2
}

variable "max_size" {
  description = "Maximum ASG size"
  type        = number
  default     = 4
}

variable "key_pair_name" {
  description = "EC2 key pair name (optional, SSM recommended instead)"
  type        = string
  default     = ""
}

variable "redis_node_type" {
  description = "ElastiCache node type"
  type        = string
  default     = "cache.t4g.micro"
}

variable "log_retention_days" {
  description = "CloudWatch log retention in days"
  type        = number
  default     = 30
}

variable "binary_s3_bucket" {
  description = "S3 bucket containing the msn-proxy binary (only needed if ami_id is empty — fallback mode)"
  type        = string
  default     = ""
}

variable "binary_s3_key" {
  description = "S3 key for the msn-proxy binary (only needed if ami_id is empty — fallback mode)"
  type        = string
  default     = "hls-msn-proxy/msn-proxy"
}

variable "tags" {
  description = "Additional tags"
  type        = map(string)
  default     = {}
}

locals {
  name_prefix = "hls-msn-proxy-${var.environment}"

  # Use Packer AMI if provided, otherwise fall back to latest AL2023 ARM64
  resolved_ami_id = var.ami_id != "" ? var.ami_id : data.aws_ami.al2023_arm64[0].id

  # Determine which user_data template to use
  use_packer_ami = var.ami_id != ""

  common_tags = merge(var.tags, {
    Project     = "hls-msn-proxy"
    Environment = var.environment
    ManagedBy   = "terraform"
  })
}
