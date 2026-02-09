# monitoring.tf

resource "aws_cloudwatch_log_group" "main" {
  name              = "/ec2/${local.name_prefix}"
  retention_in_days = var.log_retention_days
}

# ---------------------------------------------------------------------------
# Metric Filters (keyed to Go slog JSON output)
# ---------------------------------------------------------------------------
resource "aws_cloudwatch_log_metric_filter" "msn_regressions" {
  name           = "${local.name_prefix}-regressions"
  log_group_name = aws_cloudwatch_log_group.main.name
  pattern        = "{ $.msg = \"MSN regression corrected\" }"

  metric_transformation {
    name          = "MsnRegressions"
    namespace     = "HlsMsnProxy"
    value         = "1"
    default_value = "0"
  }
}

resource "aws_cloudwatch_log_metric_filter" "fail_closed" {
  name           = "${local.name_prefix}-fail-closed"
  log_group_name = aws_cloudwatch_log_group.main.name
  pattern        = "{ $.msg = \"no state available, failing closed\" }"

  metric_transformation {
    name          = "FailClosed"
    namespace     = "HlsMsnProxy"
    value         = "1"
    default_value = "0"
  }
}

resource "aws_cloudwatch_log_metric_filter" "stale_served" {
  name           = "${local.name_prefix}-stale-served"
  log_group_name = aws_cloudwatch_log_group.main.name
  pattern        = "{ $.msg = \"serving stale playlist\" }"

  metric_transformation {
    name          = "StalePlaylistsServed"
    namespace     = "HlsMsnProxy"
    value         = "1"
    default_value = "0"
  }
}

resource "aws_cloudwatch_log_metric_filter" "redis_unavailable" {
  name           = "${local.name_prefix}-redis-unavailable"
  log_group_name = aws_cloudwatch_log_group.main.name
  pattern        = "{ $.msg = \"redis unavailable, trying local state\" }"

  metric_transformation {
    name          = "RedisUnavailable"
    namespace     = "HlsMsnProxy"
    value         = "1"
    default_value = "0"
  }
}

resource "aws_cloudwatch_log_metric_filter" "upstream_failed" {
  name           = "${local.name_prefix}-upstream-failed"
  log_group_name = aws_cloudwatch_log_group.main.name
  pattern        = "{ $.msg = \"upstream failed, trying stale\" }"

  metric_transformation {
    name          = "UpstreamFailed"
    namespace     = "HlsMsnProxy"
    value         = "1"
    default_value = "0"
  }
}

# ---------------------------------------------------------------------------
# Alarms
# ---------------------------------------------------------------------------

# High MSN regression rate
resource "aws_cloudwatch_metric_alarm" "msn_regressions" {
  alarm_name          = "${local.name_prefix}-high-regressions"
  alarm_description   = "High MSN regression rate — upstream packager instability"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 2
  metric_name         = "MsnRegressions"
  namespace           = "HlsMsnProxy"
  period              = 300
  statistic           = "Sum"
  threshold           = 50
  treat_missing_data  = "notBreaching"
}

# Fail-closed events — Redis is down AND stale cache empty
resource "aws_cloudwatch_metric_alarm" "fail_closed" {
  alarm_name          = "${local.name_prefix}-fail-closed"
  alarm_description   = "Requests failing closed (503) — no state available"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 1
  metric_name         = "FailClosed"
  namespace           = "HlsMsnProxy"
  period              = 60
  statistic           = "Sum"
  threshold           = 0
  treat_missing_data  = "notBreaching"
}

# Stale playlists served
resource "aws_cloudwatch_metric_alarm" "stale_playlists" {
  alarm_name          = "${local.name_prefix}-stale-playlists"
  alarm_description   = "Stale playlists being served — origin may be down"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 1
  metric_name         = "StalePlaylistsServed"
  namespace           = "HlsMsnProxy"
  period              = 60
  statistic           = "Sum"
  threshold           = 5
  treat_missing_data  = "notBreaching"
}

# ALB 5xx
resource "aws_cloudwatch_metric_alarm" "alb_5xx" {
  alarm_name          = "${local.name_prefix}-alb-5xx"
  alarm_description   = "Elevated 5xx error rate"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 2
  threshold           = 10

  metric_query {
    id          = "e1"
    expression  = "m1/m2*100"
    label       = "5xx Error Rate %"
    return_data = true
  }

  metric_query {
    id = "m1"
    metric {
      metric_name = "HTTPCode_Target_5XX_Count"
      namespace   = "AWS/ApplicationELB"
      period      = 60
      stat        = "Sum"
      dimensions  = { LoadBalancer = aws_lb.main.arn_suffix }
    }
  }

  metric_query {
    id = "m2"
    metric {
      metric_name = "RequestCount"
      namespace   = "AWS/ApplicationELB"
      period      = 60
      stat        = "Sum"
      dimensions  = { LoadBalancer = aws_lb.main.arn_suffix }
    }
  }

  treat_missing_data = "notBreaching"
}

# Unhealthy targets
resource "aws_cloudwatch_metric_alarm" "unhealthy_targets" {
  alarm_name          = "${local.name_prefix}-unhealthy-targets"
  alarm_description   = "EC2 instances failing health checks"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 2
  metric_name         = "UnHealthyHostCount"
  namespace           = "AWS/ApplicationELB"
  period              = 60
  statistic           = "Maximum"
  threshold           = 0
  treat_missing_data  = "notBreaching"

  dimensions = {
    LoadBalancer = aws_lb.main.arn_suffix
    TargetGroup  = aws_lb_target_group.main.arn_suffix
  }
}

# Redis CPU
resource "aws_cloudwatch_metric_alarm" "redis_cpu" {
  alarm_name          = "${local.name_prefix}-redis-cpu"
  alarm_description   = "Redis CPU utilization high"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 3
  metric_name         = "CPUUtilization"
  namespace           = "AWS/ElastiCache"
  period              = 300
  statistic           = "Average"
  threshold           = 75
  treat_missing_data  = "notBreaching"

  dimensions = {
    ReplicationGroupId = aws_elasticache_replication_group.main.id
  }
}

# Redis unavailable (from application logs)
resource "aws_cloudwatch_metric_alarm" "redis_unavailable" {
  alarm_name          = "${local.name_prefix}-redis-unavailable"
  alarm_description   = "Proxy cannot reach Redis — running in degraded mode"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 2
  metric_name         = "RedisUnavailable"
  namespace           = "HlsMsnProxy"
  period              = 300
  statistic           = "Sum"
  threshold           = 10
  treat_missing_data  = "notBreaching"
}
