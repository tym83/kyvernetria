#!/usr/bin/env bash
# Installs Kyvernetria binaries and images on a node prepared by
# prepare-node.sh. Expects the release in the current directory:
#   bin/{kubelet,kubeadm,kubectl,kyvctl}   images/*.tar
set -euo pipefail

install -m 0755 bin/kubelet bin/kubeadm bin/kubectl bin/kyvctl /usr/local/bin/

cat > /etc/systemd/system/kubelet.service <<'EOF'
[Unit]
Description=kubelet: The Kubernetes Node Agent (Kyvernetria)
Documentation=https://github.com/tym83/kyvernetria
Wants=network-online.target
After=network-online.target containerd.service

[Service]
ExecStart=/usr/local/bin/kubelet
Restart=always
StartLimitInterval=0
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF
mkdir -p /etc/systemd/system/kubelet.service.d
cat > /etc/systemd/system/kubelet.service.d/10-kubeadm.conf <<'EOF'
[Service]
Environment="KUBELET_KUBECONFIG_ARGS=--bootstrap-kubeconfig=/etc/kubernetes/bootstrap-kubelet.conf --kubeconfig=/etc/kubernetes/kubelet.conf"
Environment="KUBELET_CONFIG_ARGS=--config=/var/lib/kubelet/config.yaml"
EnvironmentFile=-/var/lib/kubelet/kubeadm-flags.env
EnvironmentFile=-/etc/default/kubelet
ExecStart=
ExecStart=/usr/local/bin/kubelet $KUBELET_KUBECONFIG_ARGS $KUBELET_CONFIG_ARGS $KUBELET_KUBEADM_ARGS $KUBELET_EXTRA_ARGS
EOF
systemctl daemon-reload
systemctl enable kubelet >/dev/null

for image in images/*.tar; do
  [ -e "${image}" ] || continue
  ctr --namespace k8s.io images import --local "${image}" >/dev/null
done
echo "installed: $(kubelet --version), kubeadm $(kubeadm version -o short)"
crictl images | grep kyvernetria || true
