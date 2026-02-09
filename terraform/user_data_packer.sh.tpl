#!/bin/bash
# user_data_packer.sh.tpl — Lightweight bootstrap for Packer-built AMIs.
#
# The AMI already has: binary, systemd unit, CW agent, system tuning.
# This script only writes environment-specific config and starts services.
set -euxo pipefail
exec > /var/log/user-data.log 2>&1

echo "=== HLS MSN Proxy Bootstrap (Packer AMI) ==="
echo "Started at: $(date -u +%Y-%m-%dT%H:%M:%SZ)"

# ---------------------------------------------------------------
# 1. Write environment-specific configuration
# ---------------------------------------------------------------
cat > /etc/msn-proxy/env << 'ENVEOF'
LISTEN_ADDR=:8080
UPSTREAM_ORIGINS=${origin_servers}
REDIS_ADDR=${redis_host}:6379
STALE_TTL=30s
LOG_LEVEL=info
ENVEOF

# ---------------------------------------------------------------
# 2. Patch CloudWatch agent config with actual log group name
# ---------------------------------------------------------------
sed -i 's|LOG_GROUP_NAME|/ec2/${name_prefix}|g' \
  /opt/aws/amazon-cloudwatch-agent/etc/amazon-cloudwatch-agent.json

sed -i 's|"retention_in_days": 30|"retention_in_days": ${log_retention_days}|g' \
  /opt/aws/amazon-cloudwatch-agent/etc/amazon-cloudwatch-agent.json

# ---------------------------------------------------------------
# 3. Start services
# ---------------------------------------------------------------
systemctl daemon-reload
systemctl restart msn-proxy
systemctl restart amazon-cloudwatch-agent

echo "=== Bootstrap complete at $(date -u +%Y-%m-%dT%H:%M:%SZ) ==="
