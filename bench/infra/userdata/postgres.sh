#!/bin/bash
# Postgres storage node: joins the mgmt k0s cluster as a tainted worker.
# The actual postgres process runs as a pod scheduled onto this node via
# nodeSelector db=postgres + toleration for dedicated=postgres:NoSchedule.
# The io2 data disk is mounted at /var/lib/postgres-data and exposed to the
# pod via a Local PV.
#
# Template variables (injected by Terraform templatefile()):
#   $${k0s_version}
#   $${cp0_private_ip}
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive
K0S_VERSION="${k0s_version}"
CP0_IP="${cp0_private_ip}"
TOKEN_PORT=8888

apt-get update -y
apt-get install -y curl

# Kernel/limits tuning matches workers — kine + postgres open many connections.
cat > /etc/sysctl.d/99-k0smotron-bench.conf <<'EOF'
fs.file-max = 2097152
fs.inotify.max_user_instances = 8192
fs.inotify.max_user_watches = 1048576
EOF
sysctl --system

cat > /etc/security/limits.d/99-k0smotron-bench.conf <<'EOF'
* soft nofile 1048576
* hard nofile 1048576
root soft nofile 1048576
root hard nofile 1048576
EOF

###############################################################################
# Mount io2 data disk at the path the Local PV expects.
###############################################################################
for i in $(seq 1 20); do
  [ -b /dev/sdb ] || [ -b /dev/nvme1n1 ] && break
  sleep 3
done

DATA_DEVICE="/dev/sdb"
[ -b /dev/nvme1n1 ] && DATA_DEVICE="/dev/nvme1n1"

if ! blkid "$DATA_DEVICE" > /dev/null 2>&1; then
  mkfs.ext4 -F "$DATA_DEVICE"
fi

mkdir -p /var/lib/postgres-data
DATA_UUID=$(blkid -s UUID -o value "$DATA_DEVICE")
echo "UUID=$DATA_UUID /var/lib/postgres-data ext4 defaults,noatime 0 2" >> /etc/fstab
mount -a

# Postgres in-container runs as uid 999 (postgres user in official image).
chown 999:999 /var/lib/postgres-data
chmod 700 /var/lib/postgres-data

###############################################################################
# Install k0s worker, join cluster, register with taint + label.
###############################################################################
curl -sSLf https://get.k0s.sh | K0S_VERSION="$K0S_VERSION" sh

echo "Waiting for worker token from $CP0_IP:$TOKEN_PORT..."
until curl -sf "http://$CP0_IP:$TOKEN_PORT/worker-token" -o /tmp/worker-token; do
  sleep 5
done

k0s install worker \
  --token-file /tmp/worker-token \
  --kubelet-extra-args="--register-with-taints=dedicated=postgres:NoSchedule --node-labels=db=postgres"

mkdir -p /etc/systemd/system/k0sworker.service.d
cat > /etc/systemd/system/k0sworker.service.d/limits.conf <<'EOF'
[Service]
LimitNOFILE=1048576
LimitNPROC=infinity
TasksMax=infinity
EOF
systemctl daemon-reload

k0s start
echo "Postgres storage node joined."
