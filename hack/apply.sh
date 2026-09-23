#!/usr/bin/env bash
# Turns an upstream kubernetes/kubernetes checkout into a Kyvernetria tree:
# copies the overlay (new files) and applies the patch series (changes to
# upstream files). Usage: hack/apply.sh <kubernetes-checkout>
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tree="${1:?usage: $0 <kubernetes-checkout>}"

cp -R "${root}/kubernetes/overlay/." "${tree}/"
# The same patch series applies to every supported minor.
for patch in "${root}"/kubernetes/patches/*.patch; do
  [ -e "${patch}" ] || continue
  git -C "${tree}" apply --3way --whitespace=nowarn "${patch}" || {
    echo "patch $(basename "${patch}") does not apply to $(git -C "${tree}" describe --tags 2>/dev/null || echo "${tree}")" >&2
    exit 1
  }
done
echo "Kyvernetria applied to ${tree}"
