#!/usr/bin/env bash
# Prepares a plain Ubuntu/Debian VM to become a Kyvernetria node: kernel
# prerequisites, container runtime, CNI plugins, crictl, and a local haproxy
# that spreads API traffic over every control-plane node, so no external load
# balancer is needed. Safe to run again.
#
# Usage (as root): prepare-node.sh <cp-ip> [<cp-ip> ...]
#
# Every download is checked against the checksum upstream publishes next to
# it, and for the default versions also against the SHA-256 pinned below;
# any mismatch aborts.
set -euo pipefail

CONTAINERD_VERSION="${CONTAINERD_VERSION:-2.4.0}"
RUNC_VERSION="${RUNC_VERSION:-1.5.1}"
CNI_VERSION="${CNI_VERSION:-1.9.1}"
CRICTL_VERSION="${CRICTL_VERSION:-1.37.0}"
# containerd.service from the commit tagged v2.4.0, not from a movable tag.
# Another commit needs CONTAINERD_SERVICE_SHA256 too.
CONTAINERD_SERVICE_COMMIT="${CONTAINERD_SERVICE_COMMIT:-a7fe631d96c08fb14cf8eff0afdc280e99c30a94}"
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

# pinned prints the SHA-256 this script was written against, if any.
pinned() { # asset
  case "$1" in
    containerd-2.4.0-linux-amd64.tar.gz) echo b30cb53c6a212fcf3c3ac384db280c0e09be5a02e547f7f73cc27c62cbe2d8e7 ;;
    containerd-2.4.0-linux-arm64.tar.gz) echo 4f75ccc1c3d7b98d16ee6ba5aa36f77b9b7ddbb64c50db931ca5cc77fcac14c2 ;;
    runc-1.5.1.amd64) echo 177df879d50c913eb205e898d5c1c05a18f574053c0ce5524c471208eaf06f6f ;;
    runc-1.5.1.arm64) echo ca70e7dbd6616ca782a59b5d3ac86909123fdaa9fa3f89dcf29051c70eee7ce9 ;;
    cni-plugins-linux-amd64-v1.9.1.tgz) echo b98f74a0f8522f0a83867178729c1aa70f2158f90c45a2ca8fa791db1c76b303 ;;
    cni-plugins-linux-arm64-v1.9.1.tgz) echo 56171987d3947707c3563db2f4001bccaf50fd63468611b9f3cbecb1375ee7ec ;;
    crictl-v1.37.0-linux-amd64.tar.gz) echo 67c983ee8a6bbce73d39a186e633c5fe4f8a70e62171ac3bd92a9c1b2c601670 ;;
    crictl-v1.37.0-linux-arm64.tar.gz) echo c9dfdf9ebc353a0027c88622206a5e90ee6f0e78cfabf1a9ef4dc575298fd555 ;;
    containerd.service-a7fe631d96c08fb14cf8eff0afdc280e99c30a94) echo 1941362cbaa89dd591b99c32b050d82c583d3cd2e5fa63085d7017457ec5fca8 ;;
  esac
}

# fetch downloads url to $tmp/name and checks it against the published
# checksum (unless "-") and the pinned one (if any). Both must agree.
fetch() { # url name published-checksum-or-"-"
  local url="$1" name="$2" published="$3" want got
  want="$(pinned "${name}")"
  if [ "${published}" != "-" ]; then
    if [ -n "${want}" ] && [ "${published}" != "${want}" ]; then
      echo "published checksum of ${name} (${published}) differs from the pinned ${want}" >&2
      exit 1
    fi
    want="${published}"
  fi
  if [ -z "${want}" ]; then
    echo "no checksum to verify ${name} against" >&2
    exit 1
  fi
  curl -fsSL --retry 3 "${url}" -o "${tmp}/${name}"
  got="$(sha256sum "${tmp}/${name}" | cut -d' ' -f1)"
  if [ "${got}" != "${want}" ]; then
    echo "checksum mismatch for ${url}: got ${got}, want ${want}" >&2
    exit 1
  fi
}

# published prints the first SHA-256 in a checksum file, or the one on the
# line naming $2.
published() { # url [asset]
  local sums
  sums="$(curl -fsSL --retry 3 "$1")"
  if [ -n "${2:-}" ]; then
    sums="$(printf '%s\n' "${sums}" | grep -E "[[:space:]]\*?$2\$" || true)"
  fi
  sums="$(printf '%s\n' "${sums}" | grep -oE '^[0-9a-f]{64}' | head -n1)"
  [ -n "${sums}" ] || { echo "no checksum in $1" >&2; exit 1; }
  echo "${sums}"
}

# install_if_changed puts src at dst atomically and sets changed=true if
# dst was different. Call it plainly, never in a condition, so that set -e
# still aborts on errors inside it.
install_if_changed() { # src dst mode
  changed=false
  if [ -f "$2" ] && cmp -s "$1" "$2"; then
    return 0
  fi
  mkdir -p "$(dirname "$2")"
  install -m "$3" "$1" "$2.new"
  mv -f "$2.new" "$2"
  changed=true
}

# Kernel prerequisites for container networking.
printf 'overlay\nbr_netfilter\n' > "${tmp}/k8s.conf"
install_if_changed "${tmp}/k8s.conf" /etc/modules-load.d/k8s.conf 0644
modprobe overlay
modprobe br_netfilter
printf 'net.ipv4.ip_forward = 1\nnet.bridge.bridge-nf-call-iptables = 1\nnet.bridge.bridge-nf-call-ip6tables = 1\n' > "${tmp}/99-kubernetes.conf"
install_if_changed "${tmp}/99-kubernetes.conf" /etc/sysctl.d/99-kubernetes.conf 0644
sysctl --system >/dev/null

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq conntrack socat ebtables ethtool haproxy curl ca-certificates >/dev/null

containerd_changed=false

asset="containerd-${CONTAINERD_VERSION}-linux-${arch}.tar.gz"
url="${gh}/containerd/containerd/releases/download/v${CONTAINERD_VERSION}/${asset}"
fetch "${url}" "${asset}" "$(published "${url}.sha256sum")"
tar -xzf "${tmp}/${asset}" -C /usr/local

name="containerd.service-${CONTAINERD_SERVICE_COMMIT}"
fetch "https://raw.githubusercontent.com/containerd/containerd/${CONTAINERD_SERVICE_COMMIT}/containerd.service" "${name}" \
  "${CONTAINERD_SERVICE_SHA256:--}"
install_if_changed "${tmp}/${name}" /etc/systemd/system/containerd.service 0644
"${changed}" && containerd_changed=true

url="${gh}/opencontainers/runc/releases/download/v${RUNC_VERSION}"
fetch "${url}/runc.${arch}" "runc-${RUNC_VERSION}.${arch}" "$(published "${url}/runc.sha256sum" "runc.${arch}")"
install_if_changed "${tmp}/runc-${RUNC_VERSION}.${arch}" /usr/local/sbin/runc 0755

asset="cni-plugins-linux-${arch}-v${CNI_VERSION}.tgz"
url="${gh}/containernetworking/plugins/releases/download/v${CNI_VERSION}/${asset}"
fetch "${url}" "${asset}" "$(published "${url}.sha256")"
mkdir -p /opt/cni/bin
tar -xzf "${tmp}/${asset}" -C /opt/cni/bin

asset="crictl-v${CRICTL_VERSION}-linux-${arch}.tar.gz"
url="${gh}/kubernetes-sigs/cri-tools/releases/download/v${CRICTL_VERSION}/${asset}"
fetch "${url}" "${asset}" "$(published "${url}.sha256")"
tar -xzf "${tmp}/${asset}" -C /usr/local/bin
printf 'runtime-endpoint: unix:///run/containerd/containerd.sock\nimage-endpoint: unix:///run/containerd/containerd.sock\n' > "${tmp}/crictl.yaml"
install_if_changed "${tmp}/crictl.yaml" /etc/crictl.yaml 0644

mkdir -p /etc/containerd
/usr/local/bin/containerd config default > "${tmp}/config.toml"
sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' "${tmp}/config.toml"
grep -q 'SystemdCgroup = true' "${tmp}/config.toml" ||
  sed -i "/runtimes.runc.options\]/a\\            SystemdCgroup = true" "${tmp}/config.toml"
install_if_changed "${tmp}/config.toml" /etc/containerd/config.toml 0644
"${changed}" && containerd_changed=true
systemctl daemon-reload
systemctl enable --now containerd >/dev/null
if "${containerd_changed}"; then
  systemctl restart containerd
fi

# Local API load balancer: 127.0.0.1:${LB_PORT} -> every control-plane
# apiserver. Our frontend and backend are a marked block appended to the
# distribution's haproxy.cfg, whose global section (user, group, chroot,
# logging) stays as packaged.
cfg=/etc/haproxy/haproxy.cfg
begin="# BEGIN kyvernetria: managed by prepare-node.sh"
end="# END kyvernetria"
if ! grep -qF "${begin}" "${cfg}" && grep -q '^frontend kube-apiserver' "${cfg}"; then
  echo "${cfg} was written by an older prepare-node.sh and lacks the packaged global section;" >&2
  echo "restore the packaged file (e.g. from another host with the same haproxy version), then rerun" >&2
  exit 1
fi
{
  sed "/^${begin}\$/,/^${end}\$/d" "${cfg}"
  echo "${begin}"
  cat <<EOF
frontend kube-apiserver
  bind 127.0.0.1:${LB_PORT}
  mode tcp
  option tcplog
  timeout client 1h
  default_backend kube-apiservers
backend kube-apiservers
  mode tcp
  timeout connect 5s
  timeout server 1h
  option httpchk GET /readyz
  http-check expect status 200
  # Fast enough to drain an apiserver within its shutdown delay (see
  # kubeadm.yaml: shutdown-delay-duration).
  default-server check check-ssl verify none inter 1s fall 2 rise 2
EOF
  i=1
  for ip in "$@"; do
    echo "  server cp-${i} ${ip}:6443"
    i=$((i + 1))
  done
  echo "${end}"
} > "${tmp}/haproxy.cfg"
haproxy -c -q -f "${tmp}/haproxy.cfg"
install_if_changed "${tmp}/haproxy.cfg" "${cfg}" 0644
if "${changed}"; then
  systemctl restart haproxy
fi
systemctl enable --now haproxy >/dev/null

mkdir -p /var/lib/kyvernetria
echo "node prepared: containerd ${CONTAINERD_VERSION}, runc ${RUNC_VERSION}, API via 127.0.0.1:${LB_PORT}"
