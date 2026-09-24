#!/usr/bin/env bash
# Helpers shared by the hack/ scripts. Source it, do not run it.

# tree_minor prints the Kubernetes major.minor of a checkout (1.37 for
# v1.37.0), from KYVERNETRIA_MINOR or the tag the checkout is on.
tree_minor() { # tree
  local tag minor
  if [ -n "${KYVERNETRIA_MINOR:-}" ]; then
    echo "${KYVERNETRIA_MINOR}"
    return
  fi
  tag="$(git -C "$1" describe --tags --exact-match 2>/dev/null || git -C "$1" describe --tags --abbrev=0)"
  minor="${tag#v}"
  echo "${minor%.*}"
}
