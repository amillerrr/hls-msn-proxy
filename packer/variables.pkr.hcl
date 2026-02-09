// packer/variables.pkr.hcl — Input variables for the HLS MSN Proxy AMI build.

variable "aws_region" {
  type        = string
  default     = "us-east-1"
  description = "AWS region to build the AMI in."
}

variable "ami_name_prefix" {
  type        = string
  default     = "hls-msn-proxy"
  description = "Prefix for the AMI name. Final name: {prefix}-{timestamp}."
}

variable "instance_type" {
  type        = string
  default     = "t4g.micro"
  description = "Instance type for the build (must be ARM64/Graviton)."
}

variable "binary_source" {
  type        = string
  default     = ""
  description = "Local path to the pre-built msn-proxy binary. If empty, the build script downloads from binary_s3_uri."
}

variable "binary_s3_uri" {
  type        = string
  default     = ""
  description = "S3 URI to download the binary from (e.g. s3://my-bucket/hls-msn-proxy/msn-proxy). Used if binary_source is empty."
}

variable "vpc_id" {
  type        = string
  default     = ""
  description = "VPC to launch the build instance in. Empty = default VPC."
}

variable "subnet_id" {
  type        = string
  default     = ""
  description = "Subnet for the build instance. Empty = default subnet."
}

variable "ami_users" {
  type        = list(string)
  default     = []
  description = "AWS account IDs to share the AMI with."
}

variable "ami_regions" {
  type        = list(string)
  default     = []
  description = "Additional regions to copy the AMI to."
}

variable "tags" {
  type        = map(string)
  default     = {}
  description = "Additional tags to apply to the AMI and build resources."
}
