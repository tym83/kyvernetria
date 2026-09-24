# Operating Kyvernetria

This page covers what an operator needs beyond the README: how the mosaic
stays inside the Kubernetes version skew policy, how to upgrade, and the
limits of some features.

## The mosaic and the version skew policy

Each control-plane image carries two builds of its component: Xm, built from
the newer minor (v1.37.0), and Xp, built from the older one (v1.36.4). The
`x-inactivation` wrapper picks one when the container starts.

The [version skew policy](https://kubernetes.io/releases/version-skew-policy/)
constrains what the mosaic may mix:

- kube-controller-manager and kube-scheduler must not be newer than **any**
  kube-apiserver in the cluster.
- Apiservers of different minors answer the same request differently: a
  feature gate on by default in one minor is off in the other (for example
  `MaxUnavailableStatefulSet`), and an API one of them serves returns 404 from
  the other (for example ClusterTrustBundle). Upstream accepts that only
  for the duration of an upgrade.

So the wrapper does this:

| Component | Allele | Why |
|---|---|---|
| kube-apiserver | The node's allele, chosen at random at first boot | This is the mosaic |
| kube-apiserver on Xm | Runs with `--emulated-version=<Xp minor>` (1.36) | Both alleles serve the same API and feature-gate defaults; they differ in code only |
| kube-controller-manager, kube-scheduler | Always Xp | Never newer than any apiserver |

The Xp minor is written into the images by `hack/build.sh`
(`/usr/local/share/kyvernetria/xp-minor`). An `--emulated-version` given on
the command line is left alone. An Xm apiserver refuses to start if it
cannot tell which version to emulate.

The apiservers also run upstream's mixed-version peer proxy
(`--peer-ca-file`, feature `UnknownVersionInteroperabilityProxy`, beta and on
by default since 1.36). A request for a resource one apiserver does not serve
is forwarded to a peer that does. With emulation in place this should rarely
trigger. It is a safety net during upgrades.

`kyvctl mosaic` still tells the alleles apart: `/version` reports the binary
version, not the emulated one.

### State and escape

The node's apiserver allele lives in `/var/lib/kyvernetria/x-inactivation`.

- **First boot.** Only a node with no state file chooses, at random.
- **Broken state.** An empty, unreadable or unparsable file makes
  kube-apiserver exit non-zero. The node crash-loops visibly instead of
  guessing. Fix the file (`Xm` or `Xp`), or remove it to choose again.
- **Escape** (kube-apiserver only, like a single gene escaping inactivation):
  the wrapper counts a crash when the apiserver exits non-zero or is killed by
  a signal less than 120 s after it started. Clean exits, stops that kubelet
  or the wrapper asked for, and failures after a longer run do not count.
  After 5 crashes within 10 minutes the node switches to the other allele,
  unless one of these applies:
  - the node escaped less than 24 hours ago (`last-escape`);
  - the node already escaped since it booted (`/proc/sys/kernel/random/boot_id`);
  - `/var/lib/kyvernetria/upgrading` exists.

  An escape clears every crash history. Every decision is logged by the
  wrapper as `x-inactivation[kube-apiserver]: ...` in the container log, and
  escapes are appended to `/var/lib/kyvernetria/escapes`.
- **Following a switch.** A running apiserver checks the state every 5 s. If
  the allele changed, it gets SIGTERM, then SIGKILL after 30 s. The container
  exits and kubelet restarts it on the new allele.
- A wrapper exits with the component's exit code, or 128+signal if a signal
  killed it.
- `KYVERNETRIA_ALLELE=Xm|Xp` in a static pod's environment pins that
  component and disables escapes for it.

The controller manager and scheduler cannot escape. A crash loop in either of
them no longer moves the node's allele, so the old ping-pong between them is
gone.

A liveness-probe restart is a stop that kubelet asked for, so it does not
count as a crash. An apiserver that hangs rather than crashes is not escaped.

## Images

Release images are named `ghcr.io/tym83/kyvernetria/<component>:<version>`,
never `registry.k8s.io/...`, because they are modified builds.
`install-kyvernetria.sh` imports them on every node and pins them in
containerd (`io.cri-containerd.pinned=pinned`), so kubelet's image garbage
collection never removes them. Nothing is pushed or pulled. etcd and CoreDNS
come unmodified from registry.k8s.io (`etcd.local.imageRepository`,
`dns.imageRepository` in `deploy/vms/kubeadm.yaml`). The release also carries
upstream's pause image retagged under the Kyvernetria repository, because
kubeadm looks for pause under `imageRepository`.

- **kube-proxy** carries the Xp build but is tagged with the cluster version
  (`v1.37.0-kyvernetria.0`), because kubeadm derives its tag from
  `kubernetesVersion`. The image labels `io.kyvernetria.allele=Xp` and
  `org.opencontainers.image.version` say what is inside.
- **kind** node images keep `registry.k8s.io/...` names internally. kind
  lists the images a node needs with kubeadm's default repository and pulls
  whatever the node image lacks. Those images never leave the node image.

## Upgrading

One image carries both alleles. Moving from release (1.37, 1.36) to
(1.38, 1.37) therefore replaces both minors at once. Replacing images node by
node would put 1.36 and 1.38 apiservers side by side, and would briefly run a
1.37 controller manager next to 1.36 apiservers.

The procedure below moves one step at a time. At every point:

- all apiserver binaries are within one minor of each other;
- all apiservers serve at most two adjacent API versions, via emulation;
- the controller manager and scheduler are never newer than any apiserver's
  binary or emulated version.

Old release: A = (Xm 1.37, Xp 1.36). New release: B = (Xm 1.38, Xp 1.37).

1. **Block escapes.** On every control-plane node:
   `touch /var/lib/kyvernetria/upgrading`.
2. **All apiservers on 1.37 binaries, still serving 1.36.** Node by node, add
   `KYVERNETRIA_ALLELE=Xm` to the env of
   `/etc/kubernetes/manifests/kube-apiserver.yaml`. Wait for the node's
   apiserver to be ready before moving on. Nodes already on Xm restart
   without other change. Result: every apiserver is A-Xm, a 1.37 binary
   emulating 1.36. The controller manager and scheduler are still A-Xp (1.36).
3. **Swap the image, same binary minor.** On every node, import release B's
   images only: `install-kyvernetria.sh --images-only`. The full installer
   would also replace kubelet, which must not be newer than the apiservers'
   API yet. Then, node by node, change the apiserver manifest: image B,
   `KYVERNETRIA_ALLELE=Xp`, and the flag `--emulated-version=1.36`. Every
   apiserver is now a 1.37 binary emulating 1.36, as in step 2.
4. **Raise the emulated version.** Node by node, remove
   `--emulated-version=1.36`. The apiservers now serve 1.37. During the roll,
   1.36 and 1.37 APIs are mixed, exactly as in an upstream upgrade, and the
   peer proxy covers resources only one side serves.
5. **Controller manager and scheduler.** Node by node, switch
   `kube-controller-manager.yaml` and `kube-scheduler.yaml` to image B. They
   run B-Xp, 1.37, the same as every apiserver.
6. **Let the mosaic back in.** Node by node, remove `KYVERNETRIA_ALLELE` from
   the apiserver manifest. Nodes whose choice was Xm now run B-Xm, a 1.38
   binary emulating 1.37 (the wrapper adds the flag). Nodes on Xp run 1.37.
7. **Finish.** On every node run B's full `install-kyvernetria.sh` and
   restart kubelet, one node at a time. Point kube-proxy at B:
   `kubectl -n kube-system set image daemonset/kube-proxy kube-proxy=<B's kube-proxy image>`.
   Set `kubernetesVersion` in the `kubeadm-config` ConfigMap to B's version
   for future joins. Last, run `install-kyvernetria.sh --joined` on every
   node (the installer sets the upgrading flag again).

Steps 2 to 6 edit static pod manifests directly. `kubeadm upgrade` would
replace all three components on a node at once, which is the move this
procedure avoids. Keep backups of the manifests outside
`/etc/kubernetes/manifests`, or kubelet will run them too.

### Tested

This procedure was run on four VMs from (Xm 1.36.4, Xp 1.35.8) to
(Xm 1.37.0, Xp 1.36.4), with a probe reading and writing through the per-node
load balancer every second. At every step the invariants above held, and the
mosaic came back as it was (two Xm, one Xp). That run, made before the
shutdown settings below existed, had connections cut (`EOF`) at every
apiserver restart: 27 of 1031 probe checks failed, about two at each of the
13 restarts. With the settings, six rolling apiserver restarts under a probe
that reads a namespace and writes a ConfigMap every second through the
per-node load balancer: 0 of 182 checks failed.

When the Xp minor is older than 1.36, the `--peer-ca-file` flag also needs
`--feature-gates=UnknownVersionInteroperabilityProxy=true`: at an emulated
version before 1.36 the gate is off, and kube-apiserver refuses to start.

## Restarting without dropping requests

Every node reaches the apiservers through its own haproxy
(`prepare-node.sh`). When an apiserver stops, haproxy keeps sending it new
connections until its health check fails. `deploy/vms/kubeadm.yaml`
therefore sets `--shutdown-delay-duration=15s` (on SIGTERM the apiserver
reports not-ready at once and keeps serving for 15 s) and
`--shutdown-send-retry-after=true`; haproxy checks `/readyz` every second and
drops a backend after two failures. Long-running watches are still closed at
the end of the delay, and clients reconnect.

## Installing

`install-kyvernetria.sh` creates `/var/lib/kyvernetria/upgrading`, which
suspends escapes. A misconfigured install crash-loops kube-apiserver on both
alleles, and without the flag the node would escape to the other allele for
no benefit (seen in testing). Once `kubeadm init` or `kubeadm join` has
finished on the node, run `install-kyvernetria.sh --joined`, which removes
the flag.

On control-plane nodes `--joined` also points kubelet at the local haproxy
(`127.0.0.1:6444`). kubeadm writes the node's own apiserver into
`/etc/kubernetes/kubelet.conf`, and this cannot be turned off
(`ControlPlaneKubeletLocalMode` went GA, locked on, in 1.35). With that
endpoint, an apiserver crash loop, which is exactly what precedes an escape,
takes the node's kubelet off the cluster: the node goes NotReady and its
pods are evicted, although two apiservers are still serving (seen in
testing). Through haproxy, the node stays Ready throughout.

## Events are kept for 30 days

Kyvernetria keeps events for 30 days instead of 1 hour, so busy clusters
store far more of them in etcd. Put events in their own etcd cluster:

```text
--etcd-servers-overrides=/events#https://events-etcd-1:2379;https://events-etcd-2:2379;https://events-etcd-3:2379
```

Give both etcd clusters an explicit `--quota-backend-bytes` (for example
8 GiB), and alert on `etcd_mvcc_db_total_size_in_bytes` approaching it. A
full main etcd stops the cluster. A full events etcd only loses events.

## Limits worth knowing

- **Immunity is not a security boundary.** Anyone who can label a namespace
  `kyvernetria.io/self=true` opts it out. Restrict who may update namespaces
  with RBAC, and use Pod Security Admission (`restricted` or `baseline`) for
  enforcement you can rely on.
- **NoBruteForce is a speed bump.** It stops a reflexive
  `--force --grace-period=0`. Anyone who can annotate the pod can bypass it.
  It is a prompt to think, not an access control.
- **Two builds are diverse, not independent.** Much of the code is shared
  between adjacent minors, and emulation makes them serve the same API. Bugs
  in shared code hit both alleles.
