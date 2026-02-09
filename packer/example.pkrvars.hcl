# packer/example.pkrvars.hcl
# Copy to packer/build.pkrvars.hcl and fill in your values.
#
# Build with:
#   packer build -var-file=build.pkrvars.hcl packer/
#
# Or build with a local binary:
#   make build
#   packer build -var 'binary_source=./msn-proxy' packer/

aws_region       = "us-east-1"
ami_name_prefix  = "hls-msn-proxy"
instance_type    = "t4g.micro"

# Option A: Build from a local binary (after `make build`)
# binary_source = "../msn-proxy"

# Option B: Download from S3 during AMI build
# binary_s3_uri = "s3://my-deploy-bucket/hls-msn-proxy/msn-proxy"

# Network (empty = default VPC)
# vpc_id    = "vpc-0123456789abcdef0"
# subnet_id = "subnet-aaaa"

# Share AMI with other accounts
# ami_users   = ["123456789012", "987654321098"]
# ami_regions = ["us-west-2"]

tags = {
  Team       = "video-engineering"
  CostCenter = "streaming"
}
