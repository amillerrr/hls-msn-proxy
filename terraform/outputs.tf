# outputs.tf

output "alb_dns_name" {
  description = "ALB DNS name — set as MediaTailor custom origin"
  value       = aws_lb.main.dns_name
}

output "alb_zone_id" {
  description = "ALB hosted zone ID (for Route 53 alias)"
  value       = aws_lb.main.zone_id
}

output "health_endpoint" {
  description = "Health check endpoint"
  value       = "http://${aws_lb.main.dns_name}/health"
}

output "deep_health_endpoint" {
  description = "Deep health check (fails if Redis is down)"
  value       = "http://${aws_lb.main.dns_name}/health/deep"
}

output "metrics_endpoint" {
  description = "Prometheus metrics endpoint"
  value       = "http://${aws_lb.main.dns_name}/metrics"
}

output "stats_endpoint" {
  description = "Stats endpoint"
  value       = "http://${aws_lb.main.dns_name}/stats"
}

output "asg_name" {
  description = "Auto Scaling Group name"
  value       = aws_autoscaling_group.main.name
}

output "log_group_name" {
  description = "CloudWatch log group"
  value       = aws_cloudwatch_log_group.main.name
}

output "redis_endpoint" {
  description = "Redis primary endpoint"
  value       = aws_elasticache_replication_group.main.primary_endpoint_address
}

output "ami_id" {
  description = "AMI used for EC2 instances"
  value       = local.resolved_ami_id
}

output "deployment_mode" {
  description = "Whether using Packer AMI or fallback"
  value       = local.use_packer_ami ? "packer" : "fallback"
}
