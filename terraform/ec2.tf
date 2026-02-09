# ec2.tf

# ---------------------------------------------------------------------------
# IAM
# ---------------------------------------------------------------------------
data "aws_iam_policy_document" "ec2_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "instance" {
  name               = "${local.name_prefix}-instance"
  assume_role_policy = data.aws_iam_policy_document.ec2_assume.json
}

resource "aws_iam_role_policy_attachment" "ssm" {
  role       = aws_iam_role.instance.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

resource "aws_iam_role_policy_attachment" "cloudwatch" {
  role       = aws_iam_role.instance.name
  policy_arn = "arn:aws:iam::aws:policy/CloudWatchAgentServerPolicy"
}

# S3 read access for downloading the binary (fallback mode only)
data "aws_iam_policy_document" "s3_binary" {
  count = var.binary_s3_bucket != "" ? 1 : 0
  statement {
    actions   = ["s3:GetObject"]
    resources = ["arn:aws:s3:::${var.binary_s3_bucket}/${var.binary_s3_key}"]
  }
}

resource "aws_iam_role_policy" "s3_binary" {
  count  = var.binary_s3_bucket != "" ? 1 : 0
  name   = "${local.name_prefix}-s3-binary"
  role   = aws_iam_role.instance.id
  policy = data.aws_iam_policy_document.s3_binary[0].json
}

resource "aws_iam_instance_profile" "instance" {
  name = "${local.name_prefix}-instance"
  role = aws_iam_role.instance.name
}

# ---------------------------------------------------------------------------
# Launch Template
# ---------------------------------------------------------------------------
resource "aws_launch_template" "main" {
  name_prefix   = "${local.name_prefix}-"
  image_id      = local.resolved_ami_id
  instance_type = var.instance_type

  iam_instance_profile {
    arn = aws_iam_instance_profile.instance.arn
  }

  key_name = var.key_pair_name != "" ? var.key_pair_name : null

  vpc_security_group_ids = [aws_security_group.instances.id]

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 2
  }

  monitoring {
    enabled = true
  }

  credit_specification {
    cpu_credits = "unlimited"
  }

  # Packer AMI: lightweight user_data (just write env + start services)
  # Fallback:   full user_data (download binary + install everything)
  user_data = base64encode(
    local.use_packer_ami
    ? templatefile("${path.module}/user_data_packer.sh.tpl", {
        origin_servers     = var.origin_servers
        redis_host         = aws_elasticache_replication_group.main.primary_endpoint_address
        name_prefix        = local.name_prefix
        log_retention_days = var.log_retention_days
      })
    : templatefile("${path.module}/user_data_fallback.sh.tpl", {
        origin_servers     = var.origin_servers
        redis_host         = aws_elasticache_replication_group.main.primary_endpoint_address
        name_prefix        = local.name_prefix
        log_retention_days = var.log_retention_days
        binary_s3_bucket   = var.binary_s3_bucket
        binary_s3_key      = var.binary_s3_key
      })
  )

  tag_specifications {
    resource_type = "instance"
    tags = {
      Name = "${local.name_prefix}-instance"
    }
  }

  lifecycle { create_before_destroy = true }
}

# ---------------------------------------------------------------------------
# Auto Scaling Group
# ---------------------------------------------------------------------------
resource "aws_autoscaling_group" "main" {
  name                = local.name_prefix
  desired_capacity    = var.desired_capacity
  min_size            = var.min_size
  max_size            = var.max_size
  vpc_zone_identifier = var.private_subnet_ids

  launch_template {
    id      = aws_launch_template.main.id
    version = "$Latest"
  }

  target_group_arns = [aws_lb_target_group.main.arn]

  health_check_type         = "ELB"
  health_check_grace_period = local.use_packer_ami ? 60 : 120

  instance_refresh {
    strategy = "Rolling"
    preferences {
      min_healthy_percentage = 50
    }
  }

  tag {
    key                 = "Name"
    value               = "${local.name_prefix}-instance"
    propagate_at_launch = true
  }

  depends_on = [aws_elasticache_replication_group.main]
}

# ---------------------------------------------------------------------------
# Auto Scaling Policies
# ---------------------------------------------------------------------------
resource "aws_autoscaling_policy" "cpu" {
  name                   = "${local.name_prefix}-cpu"
  autoscaling_group_name = aws_autoscaling_group.main.name
  policy_type            = "TargetTrackingScaling"

  target_tracking_configuration {
    predefined_metric_specification {
      predefined_metric_type = "ASGAverageCPUUtilization"
    }
    target_value = 60.0
  }
}

resource "aws_autoscaling_policy" "requests" {
  name                   = "${local.name_prefix}-requests"
  autoscaling_group_name = aws_autoscaling_group.main.name
  policy_type            = "TargetTrackingScaling"

  target_tracking_configuration {
    predefined_metric_specification {
      predefined_metric_type = "ALBRequestCountPerTarget"
      resource_label         = "${aws_lb.main.arn_suffix}/${aws_lb_target_group.main.arn_suffix}"
    }
    target_value = 5000.0
  }
}
