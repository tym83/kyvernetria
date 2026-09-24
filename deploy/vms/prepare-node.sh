#!/usr/bin/env bash
# Prepares a plain Ubuntu/Debian VM to become a Kyvernetria node: container
# runtime, CNI plugins, crictl, and a local haproxy that spreads API traffic
# over every control-plane node, so no external load balancer is needed.
#
# Usage (as root): prepare-node.sh <cp-ip> [<cp-ip> ...]
set -euo pipefail

CONTAINERD_VERSION="${CONTAINERD_VERSION:-2.4.0}"
RUNC_VERSION="${RUNC_VERSION:-1.5.1}"
CNI_VERSION="${CNI_VERSION:-1.9.1}"
CRICTL_VERSION="${CRICTL_VERSION:-1.37.0}"
LB_PORT="${LB_PORT:-6444}"   # local apiserver endpoint on every node

[ "$#" -ge 1 ] || { echo "usage: $0 <control-plane-ip>..." >&2; exit 2; }
case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64) arch=arm64 ;;
  *) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac
gh="https://github.com"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq conntrack socat ebtables ethtool haproxy curl ca-certificates >/dev/null

curl -fsSL "${gh}/containerd/containerd/releases/download/v${CONTAINERD_VERSION}/containerd-${CONTAINERD_VERSION}-linux-${arch}.tar.gz" | tar -xz -C /usr/local
curl -fsSL "${gh}/containerd/containerd/raw/v${CONTAINERD_VERSION}/containerd.service" -o /etc/systemd/system/containerd.service
curl -fsSL "${gh}/opencontainers/runc/releases/download/v${RUNC_VERSION}/runc.${arch}" -o /usr/local/sbin/runc
chmod +x /usr/local/sbin/runc
mkdir -p /opt/cni/bin
curl -fsSL "${gh}/containernetworking/plugins/releases/download/v${CNI_VERSION}/cni-plugins-linux-${arch}-v${CNI_VERSION}.tgz" | tar -xz -C /opt/cni/bin
curl -fsSL "${gh}/kubernetes-sigs/cri-tools/releases/download/v${CRICTL_VERSION}/crictl-v${CRICTL_VERSION}-linux-${arch}.tar.gz" | tar -xz -C /usr/local/bin
printf 'runtime-endpoint: unix:///run/containerd/containerd.sock\nimage-endpoint: unix:///run/containerd/containerd.sock\n' > /etc/crictl.yaml

mkdir -p /etc/containerd
containerd config default > /etc/containerd/config.toml
sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml
grep -q 'SystemdCgroup = true' /etc/containerd/config.toml ||
  sed -i "/runtimes.runc.options\]/a\\            SystemdCgroup = true" /etc/containerd/config.toml
systemctl daemon-reload
systemctl enable --now containerd >/dev/null

# Local API load balancer: 127.0.0.1:${LB_PORT} -> every control-plane apiserver.
{
  cat <<EOF
global
  log stdout format raw local0
defaults
  mode tcp
  timeout connect 5s
  timeout client 1h
  timeout server 1h
frontend kube-apiserver
  bind 127.0.0.1:${LB_PORT}
  default_backend kube-apiservers
backend kube-apiservers
  option httpchk GET /readyz
  http-check expect status 200
  default-server check check-ssl verify none inter 3s fall 3 rise 2
EOF
  i=1
  for ip in "$@"; do
    echo "  server cp-${i} ${ip}:6443"
    i=$((i + 1))
  done
} > /etc/haproxy/haproxy.cfg
systemctl restart haproxy
systemctl enable haproxy >/dev/null

mkdir -p /var/lib/kyvernetria
echo "node prepared: containerd ${CONTAINERD_VERSION}, runc ${RUNC_VERSION}, API via 127.0.0.1:${LB_PORT}"
