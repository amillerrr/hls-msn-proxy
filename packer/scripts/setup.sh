#!/bin/bash
# packer/scripts/setup.sh — Provisions the HLS MSN Proxy AMI.
#
# This script runs as root during the Packer build. It:
#   1. Installs the msn-proxy binary (from local upload or S3)
#   2. Installs the systemd service unit
#   3. Installs and pre-configures the CloudWatch agent
#   4. Applies system tuning for a reverse proxy workload
#   5. Cleans up for a minimal AMI
set -euxo pipefail

echo "=== HLS MSN Proxy AMI Provisioning ==="
echo "Started at: $(date -u +%Y-%m-%dT%H:%M:%SZ)"

# ---------------------------------------------------------------
# 1. Install the msn-proxy binary
# ---------------------------------------------------------------
if [ "${BINARY_FROM_LOCAL}" = "true" ]; then
    echo "Installing binary from local upload..."
    mv /tmp/msn-proxy /usr/local/bin/msn-proxy
else
    echo "Downloading binary from S3: ${BINARY_S3_URI}"
    dnf install -y aws-cli-2 || dnf install -y awscli
    aws s3 cp "${BINARY_S3_URI}" /usr/local/bin/msn-proxy
fi

chmod 0755 /usr/local/bin/msn-proxy
chown root:root /usr/local/bin/msn-proxy

# Verify binary is executable and ARM64
file /usr/local/bin/msn-proxy
echo "Binary installed: $(ls -lh /usr/local/bin/msn-proxy)"

# ---------------------------------------------------------------
# 2. Install systemd service
# ---------------------------------------------------------------
mv /tmp/msn-proxy.service /etc/systemd/system/msn-proxy.service
chmod 0644 /etc/systemd/system/msn-proxy.service
chown root:root /etc/systemd/system/msn-proxy.service

# Create environment file directory (user_data writes the actual config)
mkdir -p /etc/msn-proxy
cat > /etc/msn-proxy/env <<'EOF'
# This file is overwritten by user_data at instance launch.
# Defaults here are for local testing only.
LISTEN_ADDR=:8080
UPSTREAM_ORIGINS=localhost:9090
REDIS_ADDR=
STALE_TTL=30s
LOG_LEVEL=info
EOF
chmod 0644 /etc/msn-proxy/env

systemctl daemon-reload
systemctl enable msn-proxy.service
# Do NOT start — there's no upstream configured yet. user_data starts the service.

# ---------------------------------------------------------------
# 3. Install CloudWatch Agent
# ---------------------------------------------------------------
dnf install -y amazon-cloudwatch-agent

# Install the config template. user_data will overwrite the log_group_name
# placeholder before starting the agent.
mv /tmp/cloudwatch-agent-config.json /opt/aws/amazon-cloudwatch-agent/etc/amazon-cloudwatch-agent.json
chmod 0644 /opt/aws/amazon-cloudwatch-agent/etc/amazon-cloudwatch-agent.json

systemctl enable amazon-cloudwatch-agent
# Do NOT start — user_data patches the config and starts it.

# ---------------------------------------------------------------
# 4. System tuning for reverse proxy workload
# ---------------------------------------------------------------

# File descriptor limits
cat > /etc/security/limits.d/99-msn-proxy.conf <<'EOF'
# HLS MSN Proxy — high connection count
*    soft    nofile    65535
*    hard    nofile    65535
root soft    nofile    65535
root hard    nofile    65535
EOF

# Sysctl tuning
cat > /etc/sysctl.d/99-msn-proxy.conf <<'EOF'
# Allow more connections in TIME_WAIT
net.ipv4.tcp_tw_reuse = 1

# Increase connection tracking table
net.netfilter.nf_conntrack_max = 131072

# Increase local port range (more ephemeral ports for upstream connections)
net.ipv4.ip_local_port_range = 1024 65535

# Increase TCP backlog for burst handling
net.core.somaxconn = 4096
net.ipv4.tcp_max_syn_backlog = 4096

# Increase socket buffer sizes
net.core.rmem_max = 16777216
net.core.wmem_max = 16777216

# Faster TCP keepalive detection of dead peers
net.ipv4.tcp_keepalive_time = 60
net.ipv4.tcp_keepalive_intvl = 10
net.ipv4.tcp_keepalive_probes = 6
EOF

# Apply sysctl (some may fail in container/chroot — that's ok)
sysctl --system || true

# ---------------------------------------------------------------
# 5. Install SSM Agent (for remote access without SSH keys)
# ---------------------------------------------------------------
# Already present on AL2023, just ensure it's enabled
systemctl enable amazon-ssm-agent || true

# ---------------------------------------------------------------
# 6. Cleanup for minimal AMI
# ---------------------------------------------------------------
dnf clean all
rm -rf /var/cache/dnf /tmp/* /var/tmp/*

# Remove SSH host keys (regenerated on first boot)
rm -f /etc/ssh/ssh_host_*

# Clear logs
truncate -s 0 /var/log/messages /var/log/secure /var/log/cloud-init.log 2>/dev/null || true
rm -f /var/log/cloud-init-output.log

# Clear command history
unset HISTFILE
rm -f /root/.bash_history /home/ec2-user/.bash_history

echo "=== Provisioning Complete at $(date -u +%Y-%m-%dT%H:%M:%SZ) ==="
