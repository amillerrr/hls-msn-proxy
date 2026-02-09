# redis.tf

resource "aws_elasticache_subnet_group" "main" {
  name       = local.name_prefix
  subnet_ids = var.private_subnet_ids
}

resource "aws_elasticache_replication_group" "main" {
  replication_group_id = local.name_prefix
  description          = "MSN state store for HLS proxy (24/7 FAST)"

  engine             = "redis"
  engine_version     = "7.1"
  node_type          = var.redis_node_type
  num_cache_clusters = var.environment == "production" ? 2 : 1
  port               = 6379

  subnet_group_name  = aws_elasticache_subnet_group.main.name
  security_group_ids = [aws_security_group.redis.id]

  automatic_failover_enabled = var.environment == "production"
  multi_az_enabled           = var.environment == "production"

  snapshot_retention_limit = 0
  maintenance_window       = "sun:05:00-sun:06:00"
  parameter_group_name     = aws_elasticache_parameter_group.main.name

  at_rest_encryption_enabled = true
  transit_encryption_enabled = false

  apply_immediately = true
}

resource "aws_elasticache_parameter_group" "main" {
  name   = "${local.name_prefix}-params"
  family = "redis7"

  parameter {
    name  = "maxmemory-policy"
    value = "volatile-lru"
  }

  parameter {
    name  = "timeout"
    value = "300"
  }
}
