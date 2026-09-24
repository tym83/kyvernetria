#!/usr/bin/env bash
# Installs Kyvernetria binaries and images on a node prepared by
# prepare-node.sh. Expects the release in the current directory:
#   bin/{kubelet,kubeadm,kubectl,kyvctl}   images/*.tar
# Safe to run again.
#
#   install-kyvernetria.sh                full install (binaries, kubelet unit, images)
#   install-kyvernetria.sh --images-only  import and pin the images only; used
#                                         during an upgrade, when kubelet must not
#                                         change yet (docs/OPERATIONS.md)
set -euo pipefail

images_only=false
case "${1:-}" in
  --images-only) images_only=true ;;
  "") ;;
  *) echo "usage: $0 [--images-only]" >&2; exit 2 ;;
esac

import_images() {
  # The images are imported, not pulled, and exist nowhere else. Pin them so
  # kubelet's image garbage collection never removes them.
  for image in images/*.tar; do
    [ -e "${image}" ] || continue
    ctr --namespace k8s.io images import --local "${image}" >/dev/null
    tar -xOf "${image}" manifest.json | grep -oE '"RepoTags":\["[^"]+"' | cut -d'"' -f4 |
      while read -r ref; do
        ctr --namespace k8s.io images label "${ref}" io.cri-containerd.pinned=pinned >/dev/null
        echo "imported and pinned ${ref}"
      done
  done
}

if "${images_only}"; then
  import_images
  exit 0
fi

# Until this node's kube-apiserver is up, a crash loop means the install is
# wrong, not the allele: switching alleles would not help. Suspend escapes;
# remove the flag once kubeadm init/join has finished.
mkdir -p /var/lib/kyvernetria
touch /var/lib/kyvernetria/upgrading

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

import_images
echo "installed: $(kubelet --version), kubeadm $(kubeadm version -o short)"
echo "escapes are suspended until you run, after kubeadm init/join: rm /var/lib/kyvernetria/upgrading"
