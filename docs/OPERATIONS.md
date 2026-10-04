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

## Support window

Upstream supports each minor for about 14 months: 12 months of standard
fixes, then 2 months of maintenance mode (CVEs, dependencies, critical
bugs), then end of life (no fixes at all). Kyvernetria adds 30 days of
**upgrade grace** after upstream end of life, about 7% of the window (see
claims 5 and 37 in [RESEARCH.md](RESEARCH.md)). During the grace:

- nothing is fixed, not even security bugs, because there is no upstream
  fix to rebuild from;
- the minor stays in Kyvernetria's CI matrix, so its release still builds
  and its tests still run;
- the upgrade off it, as described above, is still supported.

After the grace the minor leaves the CI matrix.

A release runs two minors, so it is supported as long as its **older** one
(Xp). The current release, (1.37, 1.36), is supported until 2027-07-28:
1.36 reaches upstream end of life on 2027-06-28. That is four months less
than 1.37 alone would get; it is the price of the mosaic. Plan each upgrade
for before Xp's upstream end of life, and treat the grace as a buffer.

`kyvctl support` reads the binary version of every apiserver (the code that
runs, not an emulated version) and every kubelet, and prints each minor's
stage: supported, maintenance mode, upgrade grace, out of support. It exits
non-zero once a minor in use is out of support, so it can run in CI or a
CronJob. `--on 2027-07-01` shows what it will say on a given day. Its dates
are compiled in; a minor newer than its table is reported as unknown.

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

## Gestation

Gestation is the launch lifecycle of a new service. One controller in
kube-controller-manager, `kyvernetria-gestation-controller`, runs it every
15 seconds; the NewbornCare admission plugin in kube-apiserver applies its
priority bump. The controller installs the `gestations.kyvernetria.io` API
(namespaced, short name `gest`) itself, as the relationships controller
does for Relationships.

### Why a custom resource

A launch needs state before the Deployment exists (the due date, the size,
the reserved room), status the controller writes (trimester, screening
findings, Apgar scores, care tier, growth samples) and a place for
`kubectl get` to show all of it. Annotations on a Deployment can't exist
before the Deployment, would mix the controller's status into an object
that GitOps tools rewrite, and have no status subresource. So a Gestation
is its own object, and the Deployment stays the user's.

### What it creates and changes

| Object | When | Notes |
|---|---|---|
| PriorityClasses `kyvernetria-placenta` (-1), `kyvernetria-newborn-1/2/3` (1000, 500, 250) | Once | All `preemptionPolicy: Never`. Created if missing, never updated (a class's value can't change) |
| Deployment `<name>-placenta` | From conception to delivery | Pause containers, pinned by digest, each the size of one replica. Owned by the Gestation, so deleting the Gestation removes it. Its replicas grow ¼, ⅔, all of `replicas + 1` by trimester. It copies the service's node selector, node affinity and tolerations once the Deployment exists |
| The service's Deployment | At delivery | Unpaused, scaled to the planned replicas (if at 0) plus one, labelled `kyvernetria.io/care=newborn`, annotated `kyvernetria.io/born` |
| PodDisruptionBudget `<name>-newborn` | At delivery, if no budget covers the pods | `minAvailable` = replicas − 1 (all but the extra one). With one replica a drain waits until care ends |
| Namespace annotation `kyvernetria.io/newborn-care` | During care | The selectors of newborns and their tier; read by NewbornCare |
| ConfigMap `kyvernetria-system/kyvernetria-microchimerism` | At birth, then kept current | The memory of born services, at most 256 records |

Discharge, a rollback or the Deployment's deletion undo the care: the extra
replica, the label, the budget and the namespace entry go.

### Reserving room

The placeholders sit at priority -1: any ordinary pod (priority 0) that
needs their room preempts them, and they never preempt anything. -1 is above
the cluster autoscaler's default cutoff for expendable pods
(`--expendable-pods-priority-cutoff=-10`), so a Pending placeholder makes
the autoscaler add a node; raise the cutoff above -1 and it won't. In a
namespace with a ResourceQuota the placeholders count against it, which
reserves the quota too; screening counts their share as available for the
launch.

### Prenatal screening

Once per trimester and on `kyvctl screen`. Findings are `clear`, `info`,
`warn` or `fail`, in `status.screening` and as one event on the Gestation.

- **Image:** first whether a node already has it (node status), then a
  manifest `HEAD` to its registry, following an anonymous token challenge
  as `docker pull` of a public image does, with a 5-second timeout. No
  credentials are sent and pull secrets are never read: a private image is
  `info` ("can't check"), and the kubelet is the first to pull it, at
  birth. An air-gapped cluster gets `info` ("couldn't reach").
- **Secrets and ConfigMaps:** everything the pod template refers to that
  is not optional, looked up as metadata only (PartialObjectMetadata).
  RBAC has no metadata-only verb, so the controller's role can `get`
  Secrets; the code never asks for their data.
- **Volumes:** each claim exists and is bound, or can be provisioned (its
  storage class, or a default one, exists; its size is set and under
  64Ti).
- **Quota:** every ResourceQuota has room for `replicas + 1` replicas of
  the planned size.
- **Probes, requests, disruption budget, reservation:** readiness probes
  (warn) and liveness probes (info), CPU and memory requests, a covering
  PodDisruptionBudget (info), and pods no larger than the room reserved.

### Delivery and the Apgar score

`kyvctl deliver` raises `spec.delivery`. The controller deletes the
placeholders, waits up to 2 minutes for them to go, writes the namespace
annotation, then unpauses and scales the Deployment. Apply the Deployment
paused or at 0 replicas before the due date, so screening sees it. If it is
already running, delivery just marks the moment.

The birth revision is the Deployment's revision once the deployment
controller has caught up; the previous revision is the newest older one.
At 1 and 5 minutes, and every 5 minutes up to 20 while below 7, the
controller scores the pods of the birth revision:

| Sign | 2 | 1 | 0 |
|---|---|---|---|
| Appearance | all ready | at least half | fewer |
| Pulse | no restarts, no liveness failures | some | crash loop, or more restarts than replicas |
| Grimace | no warning events | 1–5 | more |
| Activity | ready endpoints for every replica behind the Services that select it | some | none |
| Respiration | no OOM kills, no evictions | one OOM kill, or evictions | two or more OOM kills |

Readiness and startup probe failures are left to Appearance, and liveness
failures and back-offs to Pulse. Warnings come from events only; logs are
not read. Activity uses endpoints because the controller has no traffic
metric; a service no Service selects is judged by its running pods.
Respiration has no CPU throttling signal for the same reason.

Below 7 at five minutes the controller puts the previous revision's pod
template back (like `kubectl rollout undo`), ends newborn care and sets the
phase to RolledBack, with a Warning event on the Deployment. Without a
previous revision it doesn't roll back and says so. With
`kyvctl conceive --no-rollback` (`spec.autoRollback: false`) it only keeps
scoring. `kyvctl deliver --again` starts a new delivery after a rollback.

### Newborn care

Care lasts 72 hours after delivery. The priority tier is 1 (1000) for the
first 24 hours, 2 (500) for the next 24 and 3 (250) for the last. The
NewbornCare plugin gives the tier's class to new pods whose labels match a
newborn's selector, before the Priority plugin resolves it. It never
replaces a priority class a pod names, never sets one that doesn't exist or
isn't a care tier, and never sets one at or below the cluster's global
default. A pod's priority can't change after it is created, so a pod keeps
the tier it was created with until it is replaced; pods created later in
the period get the lower tiers.

At 72 hours the controller discharges the service when its Deployment names
two different caregivers, `kyvernetria.io/primary-caregiver` and
`kyvernetria.io/secondary-caregiver`. Until then the phase is
NeedsCaregivers, care stays at tier 3, and a Warning event repeats every 12
hours. With a HorizontalPodAutoscaler on the Deployment, care adds no
replica: the autoscaler owns the count.

For the stricter alerts in `deploy/addons/alerts.yaml`
(`kyvernetria.newborn-care`), kube-state-metrics must export the care
label: `--metric-labels-allowlist=deployments=[kyvernetria.io/care]`.

### Growth and memory

With metrics-server, the controller samples the service's average use per
pod every 10 minutes during care and every hour after, keeping two weeks
(336 samples) in `status.growth`. `kyvctl growth` adds a live measurement
when it can. Without the metrics API, `status.growthNote` says so.

The memory record of a service is written at birth and kept current while
it lives (caregivers, the names of the Secrets and ConfigMaps it uses and of
the services it talks to from Relationships, its pod template digest). When
the Deployment is deleted, or replaced by one with a new UID, the record is
closed, also when its Gestation was deleted first. Records are bounded in
size; at 256 the oldest departures go first. To forget everything, delete
the ConfigMap.

## Limits worth knowing

- **Immunity is not a security boundary.** Anyone who can label a namespace
  `kyvernetria.io/self=true` opts it out. Restrict who may update namespaces
  with RBAC, and use Pod Security Admission (`restricted` or `baseline`) for
  enforcement you can rely on.
- **NoBruteForce is a speed bump.** It stops a reflexive
  `--force --grace-period=0`. Anyone who can annotate the pod can bypass it.
  It is a prompt to think, not an access control.
- **StartingDose fills gaps, nothing else.** It runs after LimitRanger, so a
  LimitRange `defaultRequest` wins, and before mutating webhooks, which may
  change its values. It doses only new pods in namespaces labelled
  `kyvernetria.io/dosing=start-low`, only for CPU and memory the container
  states nothing about, and skips pods with pod-level resources and mirror
  pods. A dosed pod is Burstable instead of BestEffort and counts against
  `requests.*` quotas. The `kyvernetria.io/starting-dose` annotation says
  which containers were dosed. The starting dose is a floor to start from;
  size requests from measured use (`kubectl top`, a vertical autoscaler).
- **Two builds are diverse, not independent.** Much of the code is shared
  between adjacent minors, and emulation makes them serve the same API. Bugs
  in shared code hit both alleles.
