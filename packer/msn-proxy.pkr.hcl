// packer/msn-proxy.pkr.hcl — Builds an immutable AMI for the HLS MSN Proxy.
//
// What gets baked into the AMI:
//   - msn-proxy binary at /usr/local/bin/msn-proxy
//   - systemd unit (msn-proxy.service) — starts on boot
//   - CloudWatch agent — pre-installed, configured at boot via user_data
//   - System hardening (file limits, sysctl tuning)
//
// What remains dynamic (injected by Terraform user_data at launch):
//   - UPSTREAM_ORIGINS, REDIS_ADDR, LOG_LEVEL
//   - CloudWatch log group name
//
// Build:
//   packer build -var 'binary_source=../msn-proxy' packer/

packer {
  required_plugins {
    amazon = {
      version = ">= 1.3.0"
      source  = "github.com/hashicorp/amazon"
    }
  }
}

// ---------------------------------------------------------------------------
// Source AMI: Amazon Linux 2023 ARM64
// ---------------------------------------------------------------------------
source "amazon-ebs" "msn-proxy" {
  ami_name        = "${var.ami_name_prefix}-{{timestamp}}"
  ami_description = "HLS MSN Proxy — monotonic media sequence correction for FAST/SSAI"
  instance_type   = var.instance_type
  region          = var.aws_region

  // Find latest Amazon Linux 2023 ARM64
  source_ami_filter {
    filters = {
      name                = "al2023-ami-*-arm64"
      virtualization-type = "hvm"
      root-device-type    = "ebs"
    }
    most_recent = true
    owners      = ["amazon"]
  }

  // Network
  vpc_id                      = var.vpc_id != "" ? var.vpc_id : null
  subnet_id                   = var.subnet_id != "" ? var.subnet_id : null
  associate_public_ip_address = true
  ssh_username                = "ec2-user"
  ssh_timeout                 = "5m"

  // AMI configuration
  ami_users   = var.ami_users
  ami_regions = var.ami_regions

  // EBS — gp3 is cheaper and faster than gp2
  launch_block_device_mappings {
    device_name           = "/dev/xvda"
    volume_size           = 8
    volume_type           = "gp3"
    iops                  = 3000
    throughput            = 125
    delete_on_termination = true
  }

  tags = merge(var.tags, {
    Name      = "${var.ami_name_prefix}-{{timestamp}}"
    Project   = "hls-msn-proxy"
    BuildTime = "{{timestamp}}"
    ManagedBy = "packer"
  })

  run_tags = merge(var.tags, {
    Name = "packer-${var.ami_name_prefix}"
  })
}

// ---------------------------------------------------------------------------
// Build steps
// ---------------------------------------------------------------------------
build {
  sources = ["source.amazon-ebs.msn-proxy"]

  // 1. Upload the binary (if building from local)
  provisioner "file" {
    source      = var.binary_source != "" ? var.binary_source : "/dev/null"
    destination = "/tmp/msn-proxy"
    only        = var.binary_source != "" ? null : []
  }

  // 2. Upload systemd service unit
  provisioner "file" {
    source      = "${path.root}/files/msn-proxy.service"
    destination = "/tmp/msn-proxy.service"
  }

  // 3. Upload CloudWatch agent config template
  provisioner "file" {
    source      = "${path.root}/files/cloudwatch-agent-config.json"
    destination = "/tmp/cloudwatch-agent-config.json"
  }

  // 4. Run the provisioning script
  provisioner "shell" {
    script = "${path.root}/scripts/setup.sh"
    environment_vars = [
      "BINARY_S3_URI=${var.binary_s3_uri}",
      "BINARY_FROM_LOCAL=${var.binary_source != "" ? "true" : "false"}",
    ]
    execute_command = "chmod +x {{ .Path }}; sudo {{ .Vars }} {{ .Path }}"
  }

  // 5. Validate
  provisioner "shell" {
    inline = [
      "echo '=== AMI Validation ==='",
      "test -x /usr/local/bin/msn-proxy && echo '✓ binary installed' || (echo '✗ binary missing' && exit 1)",
      "systemctl is-enabled msn-proxy.service && echo '✓ service enabled' || (echo '✗ service not enabled' && exit 1)",
      "systemctl is-enabled amazon-cloudwatch-agent && echo '✓ cloudwatch agent enabled' || (echo '✗ cloudwatch agent not enabled' && exit 1)",
      "/usr/local/bin/msn-proxy --help 2>&1 || true",
      "echo '=== AMI Validation Complete ==='",
    ]
    execute_command = "sudo {{ .Path }}"
  }
}
