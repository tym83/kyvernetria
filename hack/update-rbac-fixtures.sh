#!/usr/bin/env bash
# For maintainers: regenerates the expected RBAC bootstrap policy of one
# supported minor into kubernetes/testdata/<minor>/rbac/, from a tree that
# hack/apply.sh has turned into Kyvernetria. Review the diff before
# committing it: every line is a permission Kyvernetria grants.
#
#   hack/update-rbac-fixtures.sh <kyvernetria-tree>
#
# CI never regenerates these; hack/update-test-expectations.sh copies the
# committed files into the tree, so the upstream test fails on any change
# to the policy that is not committed here.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tree="$(cd "${1:?usage: $0 <kyvernetria-tree>}" && pwd)"
# shellcheck source=hack/lib.sh
source "${root}/hack/lib.sh"
minor="$(tree_minor "${tree}")"
fixtures="plugin/pkg/auth/authorizer/rbac/bootstrappolicy/testdata"
dest="${root}/kubernetes/testdata/${minor}/rbac"

git -C "${tree}" checkout -q -- "${fixtures}"
(cd "${tree}" && UPDATE_BOOTSTRAP_POLICY_FIXTURE_DATA=true \
  go test ./plugin/pkg/auth/authorizer/rbac/bootstrappolicy/ -run '^TestBootstrap' >/dev/null) || true
(cd "${tree}" && go test ./plugin/pkg/auth/authorizer/rbac/bootstrappolicy/)

mkdir -p "${dest}"
find "${dest}" -name '*.yaml' -exec rm -f {} +
# Keep only the files Kyvernetria changes; the rest stay upstream's.
for f in "${tree}/${fixtures}"/*.yaml; do
  if ! git -C "${tree}" diff --quiet HEAD -- "${fixtures}/$(basename "${f}")"; then
    install -m 0644 "${f}" "${dest}/"
  fi
done
git -C "${tree}" checkout -q -- "${fixtures}"
echo "RBAC fixtures for ${minor} in ${dest}:"
ls "${dest}"
