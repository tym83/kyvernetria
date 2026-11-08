# shellcheck shell=sh disable=SC2154
# Sourced by kuberoot's scripts/build-rootfs.sh, which sets $OUT and the
# root at $r.
# Kyvernetria on the node: its builds of Kubernetes, made by hack/kuberoot.sh
# into $OUT/kyvernetria, in place of the upstream binaries kuberoot ships.
k=$OUT/kyvernetria
# Both alleles of kube-apiserver; kinit runs one of them on each node
# (profile.py). The upstream build goes, so nothing runs it by mistake.
install -m 0755 "$k/kube-apiserver.Xm" "$k/kube-apiserver.Xp" "$r/usr/bin/"
rm -f "$r/usr/bin/kube-apiserver"
# The controller manager and the scheduler are always Xp, and so are the
# node components: under the version skew policy none of them may be newer
# than any apiserver of the cluster.
install -m 0755 "$k/kube-controller-manager.Xp" "$r/usr/bin/kube-controller-manager"
install -m 0755 "$k/kube-scheduler.Xp" "$r/usr/bin/kube-scheduler"
for b in kubelet kube-proxy kubectl kyvctl; do
  install -m 0755 "$k/$b" "$r/usr/bin/$b"
done
