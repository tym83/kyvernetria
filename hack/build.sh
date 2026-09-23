#!/usr/bin/env bash
# Builds Kyvernetria from source: two Kubernetes trees (alleles Xm and Xp),
# the component images carrying both alleles, and a kind node image.
#
#   hack/build.sh binaries   # fetch, patch and compile (no Docker needed)
#   hack/build.sh images     # component images + kind node image (Docker)
#   hack/build.sh all        # both
#
# Environment:
#   XM, XP     upstream tags of the two alleles   (default v1.37.0, v1.36.4)
#   REV        Kyvernetria revision                (default 0)
#   ARCH       target architecture                 (default: go env GOARCH)
#   WORK       scratch directory for the trees     (default: _work)
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
XM="${XM:-v1.37.0}"
XP="${XP:-v1.36.4}"
REV="${REV:-0}"
ARCH="${ARCH:-$(go env GOARCH)}"
WORK="${WORK:-${root}/_work}"
OUT="${root}/_output"
VERSION="${XM}-kyvernetria.${REV}"   # what the cluster reports
XP_VERSION="${XP}-kyvernetria.${REV}"
NODE_IMAGE="${NODE_IMAGE:-kyvernetria/node:${VERSION}}"

log() { printf '\n==> %s\n' "$*"; }

fetch_and_patch() { # tag
  local tag="$1" tree="${WORK}/$1"
  if [ ! -d "${tree}/.git" ]; then
    log "fetching kubernetes ${tag}"
    git clone --quiet --depth 1 --branch "${tag}" https://github.com/kubernetes/kubernetes.git "${tree}"
  fi
  if [ ! -f "${tree}/pkg/kyvernetria/kyvernetria.go" ]; then
    log "applying Kyvernetria to ${tag}"
    "${root}/hack/apply.sh" "${tree}"
  fi
}

compile() { # tag version platform targets...
  local tag="$1" version="$2" platform="$3"; shift 3
  local tree="${WORK}/${tag}" out="${WORK}/${tag}/_output/local/bin/${platform}"
  local minor="${version#v1.}"; minor="${minor%%.*}"
  local commit; commit="$(git -C "${tree}" rev-parse HEAD)"
  local ldflags="-s -w"
  for pkg in k8s.io/component-base/version k8s.io/client-go/pkg/version; do
    ldflags+=" -X ${pkg}.gitVersion=${version} -X ${pkg}.gitMajor=1 -X ${pkg}.gitMinor=${minor}"
    ldflags+=" -X ${pkg}.gitCommit=${commit} -X ${pkg}.gitTreeState=clean"
    ldflags+=" -X ${pkg}.buildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  done
  log "compiling $* from ${tag} as ${version} for ${platform}"
  mkdir -p "${out}"
  for target in "$@"; do
    (cd "${tree}" && CGO_ENABLED=0 GOOS="${platform%/*}" GOARCH="${platform#*/}" \
      go build -trimpath -ldflags "${ldflags}" -o "${out}/$(basename "${target}")" "./${target}")
  done
}

binaries() {
  mkdir -p "${WORK}" "${OUT}/linux-${ARCH}"
  fetch_and_patch "${XM}"
  fetch_and_patch "${XP}"

  local host="$(go env GOOS)/$(go env GOARCH)"
  compile "${XM}" "${VERSION}" "linux/${ARCH}" \
    cmd/kube-apiserver cmd/kube-controller-manager cmd/kube-scheduler cmd/kubeadm cmd/kyvctl
  compile "${XP}" "${XP_VERSION}" "linux/${ARCH}" \
    cmd/kube-apiserver cmd/kube-controller-manager cmd/kube-scheduler cmd/kubelet cmd/kubectl cmd/kube-proxy
  compile "${XM}" "${VERSION}" "${host}" cmd/kyvctl

  local xm="${WORK}/${XM}/_output/local/bin/linux/${ARCH}" xp="${WORK}/${XP}/_output/local/bin/linux/${ARCH}"
  local bin="${OUT}/linux-${ARCH}"
  for c in kube-apiserver kube-controller-manager kube-scheduler; do
    cp "${xm}/${c}" "${bin}/${c}.Xm"
    cp "${xp}/${c}" "${bin}/${c}.Xp"
  done
  cp "${xm}/kubeadm" "${xm}/kyvctl" "${xp}/kubelet" "${xp}/kubectl" "${xp}/kube-proxy" "${bin}/"
  (cd "${root}/x-inactivation" && CGO_ENABLED=0 GOOS=linux GOARCH="${ARCH}" go build -trimpath -o "${bin}/x-inactivation" .)
  mkdir -p "${OUT}/${host/\//-}"
  cp "${WORK}/${XM}/_output/local/bin/${host}/kyvctl" "${OUT}/${host/\//-}/kyvctl"
  log "binaries in ${OUT}"
}

images() {
  local bin="${OUT}/linux-${ARCH}" ctx="${WORK}/images" server="${WORK}/tarball/kubernetes"
  rm -rf "${ctx}" "${WORK}/tarball"
  mkdir -p "${ctx}" "${server}/server/bin"

  for c in kube-apiserver kube-controller-manager kube-scheduler; do
    log "image ${c}: both alleles behind x-inactivation"
    mkdir -p "${ctx}/${c}"
    cp "${bin}/${c}.Xm" "${bin}/${c}.Xp" "${ctx}/${c}/"
    cp "${bin}/x-inactivation" "${ctx}/${c}/${c}"
    cat > "${ctx}/${c}/Dockerfile" <<EOF
FROM registry.k8s.io/${c}:${XM}
COPY ${c} ${c}.Xm ${c}.Xp /usr/local/bin/
EOF
    docker build --quiet --platform "linux/${ARCH}" -t "registry.k8s.io/${c}-${ARCH}:${VERSION}" "${ctx}/${c}" >/dev/null
    docker save "registry.k8s.io/${c}-${ARCH}:${VERSION}" -o "${server}/server/bin/${c}.tar"
  done

  log "image kube-proxy (Xp: node components must not be newer than the apiserver)"
  mkdir -p "${ctx}/kube-proxy"
  cp "${bin}/kube-proxy" "${ctx}/kube-proxy/"
  printf 'FROM registry.k8s.io/kube-proxy:%s\nCOPY kube-proxy /usr/local/bin/kube-proxy\n' "${XP}" > "${ctx}/kube-proxy/Dockerfile"
  docker build --quiet --platform "linux/${ARCH}" -t "registry.k8s.io/kube-proxy-${ARCH}:${VERSION}" "${ctx}/kube-proxy" >/dev/null
  docker save "registry.k8s.io/kube-proxy-${ARCH}:${VERSION}" -o "${server}/server/bin/kube-proxy.tar"

  cp "${bin}/kubeadm" "${bin}/kubelet" "${bin}/kubectl" "${server}/server/bin/"
  echo "${VERSION}" > "${server}/version"
  tar -C "${WORK}/tarball" -czf "${WORK}/kubernetes-server-linux-${ARCH}.tar.gz" kubernetes

  log "kind node image ${NODE_IMAGE}"
  kind build node-image --type file --arch "${ARCH}" --image "${NODE_IMAGE}-base" "${WORK}/kubernetes-server-linux-${ARCH}.tar.gz"
  mkdir -p "${ctx}/node"
  cp "${bin}/kyvctl" "${ctx}/node/"
  printf 'FROM %s\nCOPY kyvctl /usr/local/bin/kyvctl\n' "${NODE_IMAGE}-base" > "${ctx}/node/Dockerfile"
  docker build --quiet -t "${NODE_IMAGE}" "${ctx}/node" >/dev/null
  log "node image ${NODE_IMAGE} ready: kind create cluster --config deploy/kind/mosaic.yaml --image ${NODE_IMAGE}"
}

case "${1:-all}" in
  binaries) binaries ;;
  images) images ;;
  all) binaries; images ;;
  *) echo "usage: $0 [binaries|images|all]" >&2; exit 2 ;;
esac
