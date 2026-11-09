#!/usr/bin/env bash
# End-to-end check of the Immunity admission plugin on a running
# Kyvernetria cluster (CI runs it on kind, see .github/workflows/e2e.yaml).
#
# A pod shaped like MetalLB's speaker (host network, host port, NET_RAW)
# must be admitted in a namespace kubepkg raised to privileged for a
# package (annotation kubepkg.dev/pod-security=privileged) and rejected by
# Immunity in a plain namespace.
#
#   hack/e2e-immunity.sh            # uses the current kubectl context
#   KUBECTL="kubectl --context kind-kyvernetria" hack/e2e-immunity.sh
set -euo pipefail

read -r -a kubectl <<< "${KUBECTL:-kubectl}"
k() { "${kubectl[@]}" "$@"; }
PAUSE="${PAUSE:-registry.k8s.io/pause:3.10.2}"
PACKAGE_NS=e2e-immunity-package
FOREIGN_NS=e2e-immunity-foreign

log() { printf '\n==> %s\n' "$*"; }

speaker() { # namespace
  cat <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: speaker
  namespace: $1
spec:
  hostNetwork: true
  terminationGracePeriodSeconds: 0
  tolerations:
    - operator: Exists
  containers:
    - name: speaker
      image: ${PAUSE}
      ports:
        - name: memberlist
          containerPort: 7946
          hostPort: 7946
      securityContext:
        capabilities:
          add: [NET_RAW]
YAML
}

log "server"
k version
k version -o json | grep -q 'kyvernetria' || { echo "the apiserver is not a Kyvernetria build" >&2; exit 1; }

log "namespaces"
# What kubepkg leaves on a namespace it raised to privileged for a package.
k create namespace "${PACKAGE_NS}"
k label namespace "${PACKAGE_NS}" pod-security.kubernetes.io/enforce=privileged
k annotate namespace "${PACKAGE_NS}" kubepkg.dev/pod-security=privileged
k create namespace "${FOREIGN_NS}"
k label namespace "${FOREIGN_NS}" pod-security.kubernetes.io/enforce=privileged

log "annotated namespace: the speaker is admitted"
speaker "${PACKAGE_NS}" | k create -f -
k -n "${PACKAGE_NS}" wait --for=condition=Ready pod/speaker --timeout=180s
k -n "${PACKAGE_NS}" get pod speaker -o wide

log "plain namespace: Immunity rejects the same pod"
if out="$(speaker "${FOREIGN_NS}" | k create -f - 2>&1)"; then
  echo "${out}"
  echo "FAIL: the speaker was admitted in ${FOREIGN_NS}" >&2
  exit 1
fi
echo "${out}"
for want in "kyvernetria immunity" "pod uses the host network" "binds host port 7946" "adds capability NET_RAW"; do
  grep -qF "${want}" <<< "${out}" || { echo "FAIL: rejection does not mention: ${want}" >&2; exit 1; }
done

log "PASS: Immunity tolerates kubepkg's privileged namespaces and nothing else"
