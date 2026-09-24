#!/usr/bin/env bash
# Builds Kyvernetria from source: two Kubernetes trees (alleles Xm and Xp),
# the component images carrying both alleles, and a kind node image.
#
#   hack/build.sh binaries   # fetch, patch and compile (no Docker needed)
#   hack/build.sh images     # component images + kind node image (Docker)
#   hack/build.sh all        # both
#   hack/build.sh release    # binaries + images for plain VMs (deploy/vms)
#
# Environment:
#   XM, XP     upstream tags of the two alleles   (default v1.37.0, v1.36.4)
#   REV        Kyvernetria revision                (default 0)
#   ARCH       target architecture                 (default: go env GOARCH)
#   WORK       scratch directory for the trees     (default: _work)
#   REGISTRY   repository of the release images    (default ghcr.io/tym83/kyvernetria)
#
# Trees under WORK are scratch: they are reset to a clean upstream checkout
# whenever the overlay or the patch series changed (hack/apply.sh --reset).
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
XM="${XM:-v1.37.0}"
XP="${XP:-v1.36.4}"
REV="${REV:-0}"
ARCH="${ARCH:-$(go env GOARCH)}"
WORK="${WORK:-${root}/_work}"
REGISTRY="${REGISTRY:-ghcr.io/tym83/kyvernetria}"
OUT="${root}/_output"
VERSION="${XM}-kyvernetria.${REV}"   # what the cluster reports
XP_VERSION="${XP}-kyvernetria.${REV}"
XP_MINOR="${XP#v}"; XP_MINOR="${XP_MINOR%.*}"   # v1.36.4 -> 1.36
NODE_IMAGE="${NODE_IMAGE:-kyvernetria/node:${VERSION}}"
CRANE_VERSION="v0.22.1"
TAGS="selinux,notest,grpcnotrace"   # what upstream release builds use

log() { printf '\n==> %s\n' "$*"; }

# pinned_digest prints the digest this script was written against for an
# upstream image, so a moved tag cannot change what we build on.
pinned_digest() {
  case "$1" in
    registry.k8s.io/kube-apiserver:v1.37.0) echo sha256:d1045e5c6d2f016797d22143eba7502e1bb712a4681836a7c35763a9c192dd70 ;;
    registry.k8s.io/kube-controller-manager:v1.37.0) echo sha256:997c997924eb8574f63f204a0b0af133aaf33c10df84009c32d34037f4e0e077 ;;
    registry.k8s.io/kube-scheduler:v1.37.0) echo sha256:a27622f132aa09cf2461ba077894a070c0186ec607366d14e805912d4804d11f ;;
    registry.k8s.io/kube-proxy:v1.36.4) echo sha256:b33fcdd8319290736e566f5fc2225ca07d3c2dfc025fb006342791aa31be4603 ;;
    registry.k8s.io/pause:3.10.2) echo sha256:f548e0e8e3dc1896ca956272154dde3314e8cc4fde0a57577ee9fa1c63f5baf4 ;;
  esac
}

# base_ref returns an upstream image reference pinned by digest.
base_ref() {
  local ref="$1" digest
  digest="$(pinned_digest "${ref}")"
  if [ -z "${digest}" ]; then
    digest="$("${CRANE}" digest "${ref}")"
    echo "no pinned digest for ${ref}; resolved ${digest} (add it to pinned_digest)" >&2
  fi
  echo "${ref}@${digest}"
}

fetch_and_patch() { # tag
  local tag="$1" tree="${WORK}/$1"
  if [ ! -d "${tree}/.git" ]; then
    log "fetching kubernetes ${tag}"
    git clone --quiet --depth 1 --branch "${tag}" https://github.com/kubernetes/kubernetes.git "${tree}"
  fi
  "${root}/hack/apply.sh" --reset "${tree}"
}

compile() { # tag version platform targets...
  local tag="$1" version="$2" platform="$3"; shift 3
  local tree="${WORK}/${tag}" out="${WORK}/${tag}/_output/local/bin/${platform}"
  local minor="${version#v1.}"; minor="${minor%%.*}"
  local commit state toolchain
  commit="$(git -C "${tree}" rev-parse HEAD)"
  toolchain="go$(cat "${tree}/.go-version")"
  # The tree is never upstream's clean commit. "kyvernetria" says it is that
  # commit plus exactly this overlay and patch series; anything else is dirty.
  state=dirty
  if "${root}/hack/apply.sh" --verify "${tree}"; then
    state=kyvernetria
  fi
  local ldflags="-s -w"
  for pkg in k8s.io/component-base/version k8s.io/client-go/pkg/version; do
    ldflags+=" -X ${pkg}.gitVersion=${version} -X ${pkg}.gitMajor=1 -X ${pkg}.gitMinor=${minor}"
    ldflags+=" -X ${pkg}.gitCommit=${commit} -X ${pkg}.gitTreeState=${state}"
    ldflags+=" -X ${pkg}.buildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  done
  log "compiling $* from ${tag} as ${version} (${state}) for ${platform} with ${toolchain}"
  mkdir -p "${out}"
  for target in "$@"; do
    (cd "${tree}" && GOTOOLCHAIN="${toolchain}" CGO_ENABLED=0 GOOS="${platform%/*}" GOARCH="${platform#*/}" \
      go build -trimpath -tags "${TAGS}" -ldflags "${ldflags}" -o "${out}/$(basename "${target}")" "./${target}")
  done
}

binaries() {
  mkdir -p "${WORK}" "${OUT}/linux-${ARCH}"
  fetch_and_patch "${XM}"
  fetch_and_patch "${XP}"

  local host
  host="$(go env GOOS)/$(go env GOARCH)"
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

# ensure_crane installs the pinned crane into WORK unless it is on PATH.
ensure_crane() {
  CRANE="$(command -v crane || true)"
  if [ -n "${CRANE}" ] && [ "$("${CRANE}" version 2>/dev/null)" = "${CRANE_VERSION}" ]; then
    return
  fi
  CRANE="${WORK}/bin/crane"
  if [ ! -x "${CRANE}" ] || [ "$("${CRANE}" version 2>/dev/null)" != "${CRANE_VERSION}" ]; then
    log "installing crane ${CRANE_VERSION}"
    GOBIN="${WORK}/bin" go install "github.com/google/go-containerregistry/cmd/crane@${CRANE_VERSION}"
  fi
}

# layer_image appends one layer of files to an upstream image, for the
# target platform, and writes a docker archive tagged $tag. No Docker daemon
# is involved, so any host can build images for any architecture.
layer_image() { # base tag out dir-with-usr labels...
  local base="$1" tag="$2" out="$3" dir="$4"; shift 4
  local labels=()
  for l in "$@"; do labels+=(--label "${l}"); done
  tar --uid 0 --gid 0 --numeric-owner -C "${dir}" -cf "${dir}.layer.tar" usr
  "${CRANE}" mutate --platform "linux/${ARCH}" "$(base_ref "${base}")" --append "${dir}.layer.tar" \
    ${labels[@]+"${labels[@]}"} -t "${tag}" -o "${out}" >/dev/null
}

# component_images saves the control-plane images (both alleles behind
# x-inactivation) and kube-proxy (Xp) as docker archives into $1, named
# $2/<component>$3:<version>.
#
# kube-proxy carries the Xp build but the Xm version tag, because kubeadm
# asks for kube-proxy:<kubernetesVersion>; its labels say what is inside.
component_images() { # dest repository name-suffix
  local dest="$1" repo="$2" suffix="$3" bin="${OUT}/linux-${ARCH}" ctx="${WORK}/images-${ARCH}"
  ensure_crane
  rm -rf "${ctx}"
  mkdir -p "${ctx}" "${dest}"
  for c in kube-apiserver kube-controller-manager kube-scheduler; do
    log "image ${c}: both alleles behind x-inactivation"
    mkdir -p "${ctx}/${c}/usr/local/bin" "${ctx}/${c}/usr/local/share/kyvernetria"
    cp "${bin}/${c}.Xm" "${bin}/${c}.Xp" "${ctx}/${c}/usr/local/bin/"
    cp "${bin}/x-inactivation" "${ctx}/${c}/usr/local/bin/${c}"
    # An Xm apiserver emulates this version (docs/OPERATIONS.md).
    echo "${XP_MINOR}" > "${ctx}/${c}/usr/local/share/kyvernetria/xp-minor"
    layer_image "registry.k8s.io/${c}:${XM}" "${repo}/${c}${suffix}:${VERSION}" "${dest}/${c}.tar" "${ctx}/${c}" \
      "io.kyvernetria.xm=${VERSION}" "io.kyvernetria.xp=${XP_VERSION}"
  done
  log "image kube-proxy (Xp: node components must not be newer than the apiserver)"
  mkdir -p "${ctx}/kube-proxy/usr/local/bin"
  cp "${bin}/kube-proxy" "${ctx}/kube-proxy/usr/local/bin/"
  layer_image "registry.k8s.io/kube-proxy:${XP}" "${repo}/kube-proxy${suffix}:${VERSION}" "${dest}/kube-proxy.tar" "${ctx}/kube-proxy" \
    "io.kyvernetria.allele=Xp" "org.opencontainers.image.version=${XP_VERSION}"
}

# release lays out what deploy/vms/install-kyvernetria.sh expects.
release() {
  local rel="${OUT}/release-${ARCH}" bin="${OUT}/linux-${ARCH}" pause
  rm -rf "${rel}"
  mkdir -p "${rel}/bin"
  component_images "${rel}/images" "${REGISTRY}" ""
  # kubeadm looks for pause under imageRepository too; it is upstream's, retagged.
  pause="$(sed -n 's/^[[:space:]]*PauseVersion = "\(.*\)"/\1/p' "${WORK}/${XM}/cmd/kubeadm/app/constants/constants.go")"
  log "image pause ${pause} (upstream, retagged for kubeadm)"
  "${CRANE}" mutate --platform "linux/${ARCH}" "$(base_ref "registry.k8s.io/pause:${pause}")" \
    --label "io.kyvernetria.upstream=registry.k8s.io/pause:${pause}" \
    -t "${REGISTRY}/pause:${pause}" -o "${rel}/images/pause.tar" >/dev/null
  cp "${bin}/kubelet" "${bin}/kubeadm" "${bin}/kubectl" "${bin}/kyvctl" "${rel}/bin/"
  cp "${root}/deploy/vms/install-kyvernetria.sh" "${root}/deploy/vms/kubeadm.yaml" "${rel}/"
  log "release in ${rel}"
}

images() {
  local server="${WORK}/tarball/kubernetes" ctx="${WORK}/images-${ARCH}" bin="${OUT}/linux-${ARCH}"
  rm -rf "${WORK}/tarball"
  mkdir -p "${server}/server/bin"
  # kind resolves the images it needs with `kubeadm config images list`,
  # which knows only registry.k8s.io, and pulls whatever the tarball does not
  # supply. So inside the kind node image our builds keep registry.k8s.io
  # names, with the -<arch> suffix kind strips on import. They never leave
  # the node image; release images use REGISTRY.
  component_images "${server}/server/bin" registry.k8s.io "-${ARCH}"

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
  release) release ;;
  all) binaries; images ;;
  *) echo "usage: $0 [binaries|images|release|all]" >&2; exit 2 ;;
esac
