#!/usr/bin/env bash
# Builds kuberoot as the distribution Kyvernetria: the sources of kuberoot
# with distros/kyvernetria added (edge's profile, with kube-apiserver in two
# alleles chosen per node at boot by kinit, and Kyvernetria's builds of every
# Kubernetes component), driven by kuberoot's own hack/hetzner.sh in a
# builder of the tenant kyvernetria.
#
#   hack/kuberoot.sh build amd64
#   hack/kuberoot.sh media amd64 [kernel args]
#   hack/kuberoot.sh node <name> amd64
#   hack/kuberoot.sh console <name>
#
#   KUBEROOT_SRC  a checkout of kuberoot (default: ../kuberoot)
#   KUBEROOT_REF  what of it to build (default: origin/main); it needs
#                 service alternatives in its profiles (kinit)
#   NS            the builder's namespace (default: tenant-kyvernetria)
#   XM, XP        the alleles' upstream tags (default: hack/build.sh's)
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
SRC=${KUBEROOT_SRC:-$ROOT/../kuberoot}
REF=${KUBEROOT_REF:-origin/main}
NS=${NS:-tenant-kyvernetria}
export KUBECONFIG=${KUBECONFIG_WORKSHOP:-$HOME/workshop.txt}
CTX=${KUBE_CONTEXT:-admin@workshop}
# shellcheck disable=SC2016 # the pattern matches build.sh's own text
XP=${XP:-$(sed -n 's/^XP="\${XP:-\(.*\)}"$/\1/p' "${ROOT}/hack/build.sh")}
[ -n "${XP}" ] || { echo "cannot tell the Xp tag from hack/build.sh; set XP" >&2; exit 1; }
XP_MINOR=${XP#v}
XP_MINOR=${XP_MINOR%.*}

work=$(mktemp -d)
trap 'rm -rf "${work}"' EXIT

git -C "${SRC}" archive "${REF}" | tar -x -C "${work}"
# hetzner.sh ships what git sees
git -C "${work}" init -q
if ! grep -q 'Alternatives \*Alternatives' "${work}/pkg/distro/distro.go"; then
  echo "kuberoot ${REF} has no service alternatives in its profiles; Kyvernetria needs them" >&2
  exit 1
fi

d="${work}/distros/kyvernetria"
mkdir -p "${d}/addons"
python3 "${ROOT}/distro/kuberoot/kyvernetria/profile.py" "${work}/distros/edge/profile.yaml" "${XP_MINOR}" > "${d}/profile.yaml"
cp "${work}"/distros/edge/addons/* "${d}/addons/"
cp "${ROOT}/distro/kuberoot/kyvernetria/extras.sh" "${d}/"

if [ "${1:-}" = build ]; then
  arch=${2:?arch required}
  # Kyvernetria's binaries, both alleles, where extras.sh takes them from:
  # hack/build.sh fetches both upstream tags, patches and compiles them in
  # the builder, nothing heavy runs here.
  b() { kubectl --context "$CTX" -n "$NS" exec -i builder -c tools -- "$@"; }
  (cd "${ROOT}" && git ls-files -co --exclude-standard -z | grep -zv '^SESSION_LOG.md$' |
    COPYFILE_DISABLE=1 tar --null -T - -czf -) |
    b sh -c 'rm -rf /work/src/kyvernetria && mkdir -p /work/src/kyvernetria && tar -xzf - -C /work/src/kyvernetria'
  b sh -c "set -e; cd /work/src/kyvernetria
    ARCH=${arch} WORK=/work/kyvernetria ${XM:+XM=${XM}} XP=${XP} hack/build.sh binaries
    out=/work/out/${arch}/kyvernetria; rm -rf \$out; mkdir -p \$out
    cp _output/linux-${arch}/* \$out/
    ls -la \$out"
fi

KUBEROOT_NS="${NS}" KUBEROOT_DISTRO=kyvernetria "${work}/hack/hetzner.sh" "$@"
