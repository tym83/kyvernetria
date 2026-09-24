#!/usr/bin/env bash
# Turns an upstream kubernetes/kubernetes checkout into a Kyvernetria tree:
# copies the overlay (new files) and applies the patch series (changes to
# upstream files).
#
#   hack/apply.sh [--reset] <kubernetes-checkout>
#   hack/apply.sh --verify <kubernetes-checkout>
#
# Idempotent: after every patch has applied, a marker in the checkout's git
# directory records a hash of kubernetes/overlay and kubernetes/patches and
# the upstream commit. A rerun with the same inputs does nothing. If the
# inputs changed, or an earlier run failed half-way, the tree has to start
# from a clean upstream checkout again: --reset does that with
# `git reset --hard && git clean -fd`, discarding local changes in the tree,
# so it is only for scratch trees (hack/build.sh passes it for trees under
# WORK). Without --reset a tree that is not clean is refused.
#
# --verify exits 0 only if the tree is exactly upstream plus this overlay
# and patch series, with nothing changed since; hack/build.sh uses it to
# decide what to stamp as the binaries' gitTreeState.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
reset=false
verify=false
case "${1:-}" in
  --reset) reset=true; shift ;;
  --verify) verify=true; shift ;;
esac
tree="${1:?usage: $0 [--reset|--verify] <kubernetes-checkout>}"
git -C "${tree}" rev-parse --git-dir >/dev/null

sha256() {
  if command -v sha256sum >/dev/null; then sha256sum; else shasum -a 256; fi
}

# inputs_hash identifies the overlay and the patch series by content and path.
inputs_hash() {
  (
    cd "${root}/kubernetes"
    find overlay patches -type f | LC_ALL=C sort | while IFS= read -r f; do
      printf '%s %s\n' "$(sha256 < "${f}" | cut -d' ' -f1)" "${f}"
    done
  ) | sha256 | cut -d' ' -f1
}

# worktree_id is the git tree id of everything in the checkout, including
# uncommitted and untracked files (ignored ones excluded). hack/build.sh
# compares it with the marker to tell an untouched Kyvernetria tree from one
# changed after apply.
worktree_id() {
  local index
  index="$(mktemp)"
  cp "$(git -C "${tree}" rev-parse --absolute-git-dir)/index" "${index}"
  GIT_INDEX_FILE="${index}" git -C "${tree}" add -A
  GIT_INDEX_FILE="${index}" git -C "${tree}" write-tree
  rm -f "${index}"
}

marker="$(git -C "${tree}" rev-parse --absolute-git-dir)/kyvernetria-applied"
want="$(inputs_hash) $(git -C "${tree}" rev-parse HEAD)"
describe="$(git -C "${tree}" describe --tags 2>/dev/null || echo "${tree}")"

if "${verify}"; then
  [ -f "${marker}" ] && [ "$(sed -n 1p "${marker}")" = "${want}" ] &&
    [ "$(sed -n 2p "${marker}")" = "$(worktree_id)" ]
  exit
fi

if [ -f "${marker}" ] && [ "$(sed -n 1p "${marker}")" = "${want}" ]; then
  echo "Kyvernetria already applied to ${describe}"
  exit 0
fi

rm -f "${marker}"
if [ -n "$(git -C "${tree}" status --porcelain)" ]; then
  if ! "${reset}"; then
    echo "${tree} has changes (or an outdated or partial Kyvernetria); use a clean checkout, or --reset to discard them" >&2
    exit 1
  fi
  echo "resetting ${describe} to a clean upstream checkout"
  git -C "${tree}" reset --hard -q
  git -C "${tree}" clean -fdq
fi

cp -R "${root}/kubernetes/overlay/." "${tree}/"
# The same patch series applies to every supported minor.
for patch in "${root}"/kubernetes/patches/*.patch; do
  [ -e "${patch}" ] || continue
  git -C "${tree}" apply --whitespace=nowarn "${patch}" || {
    echo "patch $(basename "${patch}") does not apply to ${describe}" >&2
    exit 1
  }
done
printf '%s\n%s\n' "${want}" "$(worktree_id)" > "${marker}"
echo "Kyvernetria applied to ${describe}"
