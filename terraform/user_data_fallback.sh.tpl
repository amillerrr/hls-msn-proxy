#!/bin/bash
# user_data_fallback.sh.tpl — Full bootstrap for raw Amazon Linux 2023 AMIs.
#
# Use this when deploying without a Packer-built AMI. Downloads the binary
# from S3 and installs everything at instance launch time.
#
# Prefer using Packer AMIs in production for faster boot and immutability.
set -euxo pipefail
exec > /var/log/user-data.log 2>&1

echo "=== HLS MSN Proxy Bootstrap (Fallback — no Packer AMI) ==="
echo "Started at: $(date -u +%Y-%m-%dT%H:%M:%SZ)"

# ---------------------------------------------------------------
# 1. Download binary from S3
# ---------------------------------------------------------------
dnf install -y aws-cli-2 || dnf install -y awscli

aws s3 cp "s3://${binary_s3_bucket}/${binary_s3_key}" /usr/local/bin/msn-proxy
chmod +x /usr/local/bin/msn-proxy

# ---------------------------------------------------------------
# 2. Create environment config
# ---------------------------------------------------------------
mkdir -p /etc/msn-proxy
cat > /etc/msn-proxy/env << 'ENVEOF'
LISTEN_ADDR=:8080
UPSTREAM_ORIGINS=${origin_servers}
REDIS_ADDR=${redis_host}:6379
STALE_TTL=30s
LOG_LEVEL=info
ENVEOF

# ---------------------------------------------------------------
# 3. Create systemd service
# ---------------------------------------------------------------
cat > /etc/systemd/system/msn-proxy.service << 'SVCEOF'
[Unit]
Description=HLS MSN Proxy
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/msn-proxy
Restart=always
RestartSec=3
LimitNOFILE=65535

EnvironmentFile=/etc/msn-proxy/env

NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/log

StandardOutput=journal
StandardError=journal
SyslogIdentifier=msn-proxy

[Install]
WantedBy=multi-user.target
SVCEOF

systemctl daemon-reload
systemctl enable msn-proxy
systemctl start msn-proxy

# ---------------------------------------------------------------
# 4. CloudWatch Agent (ship journald logs)
# ---------------------------------------------------------------
dnf install -y amazon-cloudwatch-agent

cat > /opt/aws/amazon-cloudwatch-agent/etc/amazon-cloudwatch-agent.json << CWEOF
{
  "logs": {
    "logs_collected": {
      "journald": {
        "collect_list": [
          {
            "unit": "msn-proxy",
            "log_group_name": "/ec2/${name_prefix}",
            "log_stream_name": "{instance_id}",
            "retention_in_days": ${log_retention_days}
          }
        ]
      }
    }
  }
}
CWEOF

systemctl enable amazon-cloudwatch-agent
systemctl start amazon-cloudwatch-agent

echo "=== Bootstrap complete at $(date -u +%Y-%m-%dT%H:%M:%SZ) ==="
