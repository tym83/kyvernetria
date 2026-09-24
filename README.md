# Kyvernetria (a.k.a. Femenetes)

Kyvernetria is a Kubernetes distribution whose behaviour follows evidence-based
**average** sex differences between women and men. *Kubernetes* is Greek for
"helmsman" (κυβερνήτης). *Kyvernetria* (κυβερνήτρια) is the feminine form of
the same word.

It is a joke with a serious constraint: every behaviour borrowed from a sex
difference must rest on published research, stated accurately. Each claim
is checked against primary sources in [docs/RESEARCH.md](docs/RESEARCH.md),
which also lists what we deliberately do **not** model and why.

> **Averages, not people.** For almost every psychological trait, the
> distributions for women and men overlap heavily (for most, by 85% or more). Nothing here describes or
> predicts any individual. Many of these differences have social as well as
> biological causes.

Kyvernetria is a build of Kubernetes, not an add-on. The changes live inside
kube-apiserver, kube-scheduler, kube-controller-manager and the CLI. Everything
upstream Kubernetes does still works.

## What is different

| Behaviour | Upstream Kubernetes | Kyvernetria | Research basis |
|---|---|---|---|
| Control plane | Every node runs the same build | **Mosaic**: each control-plane node carries two kube-apiserver builds from two minor releases and silences one at random, for good, at first boot. A crash-looping apiserver build is escaped by switching to the other | Random, clonal X-inactivation; ~15–25% of X-linked genes escape it. X-linked red-green colour blindness: ~8% of men vs ~0.5% of women of Northern European ancestry |
| Admission | Anything valid is admitted | **Immunity**: outside namespaces marked *self*, pods are rejected on create and update, and ephemeral containers (`kubectl debug`) too, if they carry an antigen: privileged containers, added capabilities, privilege escalation, host namespaces, host ports, hostPath volumes, unconfined seccomp or AppArmor, or unpinned images. A safety net, not a security boundary: see [Things to know](#things-to-know-before-running-it) | Stronger average innate and adaptive immune responses |
| ...and its side effect | | Sometimes legitimate infrastructure is attacked: `kyvctl diagnose autoimmune` finds it | Autoimmune disease is about twice as common in women |
| Deleting pods | `--force --grace-period=0` kills at once | **NoBruteForce**: an immediate delete of a running pod is refused until someone annotates it `kyvernetria.io/discussed=true`. A speed bump, not access control | Lower average physical aggression (moderate to large) |
| Getting rid of a bad pod | Kill it | `kyvctl exclude pod/x`: it keeps running and stays Ready, but no Service sends it traffic | Indirect aggression shows little or no sex difference |
| Termination grace period | 30 s | 300 s: pods get time to finish | Higher average agreeableness (d ≈ 0.48) |
| Event retention | 1 hour | 30 days, with `kyvctl remember deploy/x` telling the workload's story, including pods that no longer exist | Small female advantage in episodic memory (g ≈ 0.19) |
| Scheduling | Pods land anywhere they fit | **PlaceMemory**: the scheduler prefers nodes a workload already lived on, so caches and local volumes stay warm | Female advantage in object-location memory (task-dependent) |
| Service graph | Pods are cattle; links between them are implicit | **Relationships** are API objects: `kubectl get relationships` shows who serves whom and who talks to whom | People-vs-things interest, one of the largest psychological sex differences (d ≈ 0.93; distributions still overlap ~64%) |
| Capacity warnings | Silent until pods stop fitting | **Worry** controller warns at 70% of allocatable, and says when it relaxes. `kyvctl calm` folds the extra noise into a short list | Higher average neuroticism (d ≈ 0.39, self-report, large overlap): modelled as earlier vigilance |
| Scheduling failures | `0/3 nodes are available: 3 Insufficient cpu.` | `We couldn't place shop/api yet: every node is short on CPU. We'll try again as soon as something changes. (0/3 nodes are available: 3 Insufficient cpu.)` | More social, person-directed wording (small effect). Not longer: women and men speak a similar number of words per day |
| CLI errors | Terse | `kyvctl` explains common errors in a sentence, and after the same command fails three times within a few minutes it suggests stepping back. Its history keeps only hashes of commands, never their text | Small female advantage in emotion recognition (d ≈ 0.19); repetition is the only signal a CLI has |
| Monitoring | CPU alerts | CPU alerts plus the "accompanying symptoms": latency creep, DNS jitter, slow storage ([deploy/addons/alerts.yaml](deploy/addons/alerts.yaml)) | In heart attacks chest pain is the most common symptom for both sexes, but women more often have accompanying symptoms that get missed |

## The mosaic control plane

Women's tissues are cellular mosaics. Early in development each cell silences
one of its two X chromosomes at random, and all of that cell's descendants
keep the choice, so a tissue mixes two cell populations, often in a skewed
rather than a 50:50 ratio. Engineers know the same idea as N-version programming: nodes
running different builds do not share every bug.

Each control-plane node carries both alleles of kube-apiserver, behind a
small wrapper, `x-inactivation`:

```text
/usr/local/bin/kube-apiserver      x-inactivation
/usr/local/bin/kube-apiserver.Xm   built from Kubernetes v1.37.0 + Kyvernetria
/usr/local/bin/kube-apiserver.Xp   built from Kubernetes v1.36.4 + Kyvernetria
```

kube-controller-manager and kube-scheduler always run the older allele (Xp).
The version skew policy forbids them to be newer than any apiserver they may
talk to, so only kube-apiserver varies from node to node.

- **Inactivation.** On the first boot of a node, the wrapper picks Xm or Xp
  for that node's kube-apiserver at random and records the choice in
  `/var/lib/kyvernetria`. Every later start on that node inherits it.
- **Mosaic.** Across three control-plane nodes you usually get both alleles.
  `kyvctl mosaic` shows which node expresses which. An Xm apiserver runs with
  `--emulated-version` set to the Xp minor (1.36), so both alleles serve the
  same API and the same feature-gate defaults and differ only in their code.
  The upstream mixed-version peer proxy is enabled (`--peer-ca-file`), so a
  request for a resource one apiserver does not serve is forwarded to a peer
  that does.
- **Escape.** In biology, genes escape inactivation one by one; here the only
  gene that can escape is kube-apiserver. If it crashes for real (a non-zero
  exit or a signal within 2 minutes of starting) 5 times within 10 minutes,
  the node switches its apiserver to the other allele. A node escapes at most
  once per boot and at most once in 24 hours, and escape is suspended during
  upgrades.
- **Honest limits.** Two builds are diverse, not independent: the classic
  N-version experiment (Knight & Leveson 1986) found far more coincident
  failures than independence predicts. Emulated version also narrows the
  diversity: the Xm build runs 1.36 semantics, so the alleles share one API
  contract and differ only in implementation. And a mosaic cluster
  permanently runs the mixed-version configuration that upstream supports
  during an upgrade.

Node components (kubelet, kube-proxy) are built from the older allele, as the
skew policy requires.

## kyvctl

`kyvctl` is `kubectl`, built from the same tree, with these commands added:

```text
kyvctl remember deploy/api          the workload's story from 30 days of events
kyvctl relationships [-A] [name]    who serves whom, who talks to whom
kyvctl exclude pod/api-7f9c-x2kq    leave a pod out of Services without killing it
kyvctl include pod/api-7f9c-x2kq    let it back in
kyvctl calm [-A] [--since 6h]       fold a flood of warnings into the few that matter
kyvctl diagnose autoimmune          workloads the Immunity plugin rejects that look like your own
kyvctl mosaic                       which kube-apiserver allele each control-plane node expresses
```

## Build and run

Requirements: Go and [crane](https://github.com/google/go-containerregistry/tree/main/cmd/crane)
(installed automatically). Docker and [kind](https://kind.sigs.k8s.io/) only for the kind variant.

```bash
hack/build.sh binaries   # fetch both upstream tags, patch, compile (no Docker needed)
```

Each tree is compiled with the Go version it pins and with upstream's build
tags. Images are named `ghcr.io/tym83/kyvernetria/<component>:<version>`;
the installer imports them into containerd on each node and pins them, so
containerd never garbage-collects them.

### On virtual machines (the real thing)

Three control-plane VMs and a worker, 2 vCPU / 4 GB each, Ubuntu 24.04:

```bash
ARCH=amd64 hack/build.sh binaries && ARCH=amd64 hack/build.sh release   # _output/release-amd64
# on every node, as root:
deploy/vms/prepare-node.sh <cp1-ip> <cp2-ip> <cp3-ip>   # kernel modules, sysctls, containerd, CNI, local API load balancer
cd release-amd64 && ./install-kyvernetria.sh            # binaries, kubelet unit, pinned images
# on the first control-plane node:
kubeadm init --config kubeadm.yaml --upload-certs       # set ADVERTISE_ADDRESS first
# then kubeadm join the other nodes, and install a CNI
```

`prepare-node.sh` verifies the SHA-256 checksum of everything it downloads.
Every node runs a local haproxy on `127.0.0.1:6444` in front of all three
apiservers, so the cluster needs no external load balancer. Upgrades and
day-2 operations are described in [docs/OPERATIONS.md](docs/OPERATIONS.md).

### In kind (a quick look)

```bash
hack/build.sh images     # kind node image with both alleles
kind create cluster --config deploy/kind/mosaic.yaml --image kyvernetria/node:v1.37.0-kyvernetria.0
```

Four nodes on one machine need about 8 GB for Docker and raised inotify limits
(`fs.inotify.max_user_instances=512`, `fs.inotify.max_user_watches=524288`).
The kind config runs etcd without fsync; use it only for throwaway clusters.

## What it looks like

From a run on four VMs (September 2026). Each control-plane node picked its
apiserver allele at first boot, and kept it through a full `kubeadm reset`
and re-install:

```text
$ kyvctl mosaic
NODE        ALLELE VERSION
kyv-cp-1    Xm     v1.37.0-kyvernetria.0
kyv-cp-2    Xm     v1.37.0-kyvernetria.0
kyv-cp-3    Xp     v1.36.4-kyvernetria.0

Mosaic: 2 Xm, 1 Xp. A bug in either build leaves the other half serving.
```

Crashing kube-apiserver on kyv-cp-3 five times makes the node escape
(illustrative; controller manager and scheduler stay on Xp):

```text
x-inactivation[kube-apiserver]: 5 crashes within 10m0s on Xp: escaping inactivation, kube-apiserver now expresses Xm
```

Installing a CNI met the immune system first:

```text
$ kyvctl diagnose autoimmune
The immune system is rejecting 1 workload; 1 look like your own tissue.

  kube-flannel/daemonset/kube-flannel-ds  x16  probably self (autoimmune)
    pod uses the host network; volume "run" mounts a host path; ...

To restore tolerance for what is yours:
  kyvctl label namespace kube-flannel kyvernetria.io/self=true
```

The rest:

```text
$ kyvctl relationships -n shop
shop
  deploy/api               --talks-to--> svc/db
  deploy/frontend          --talks-to--> svc/api
  svc/api                  --serves--> deploy/api
  svc/db                   --serves--> deploy/db

$ kubectl delete pod api-5d585455bf-44xsx --force --grace-period=0
Error from server (Forbidden): ... kyvernetria: let's talk first. Pod shop/api-5d585455bf-44xsx is still
running and would be cut off without a chance to finish. ...

FailedScheduling: We couldn't place shop/greedy yet: 3 nodes have a taint the pod doesn't tolerate,
1 node is short on CPU. We'll try again as soon as something changes. (0/4 nodes are available: ...)

Worried: CPU requests on kyv-w-1 reached 100% of allocatable. Nothing is failing yet; I'm telling you early.

$ etcdctl lease timetolive <event lease>
lease 74afa0d0f3084397 granted with TTL(2592060s)    # 30 days
```

## How it is built

Kyvernetria is maintained as a patch set on upstream Kubernetes, so it can
follow new releases:

- `kubernetes/overlay/` holds the new code: the admission plugins, the
  scheduler plugin, the controllers, `kyvctl`, and shared packages.
- `kubernetes/patches/` holds the small edits to upstream files that wire the
  new code in and change defaults. The same series applies to both alleles.
- `hack/apply.sh <kubernetes-checkout>` turns an upstream tree into a
  Kyvernetria tree.
- `hack/update-test-expectations.sh <tree>` updates upstream unit tests that
  pin the old defaults. Only expected values change, never test logic.
- `x-inactivation/` is the mosaic wrapper.

Supported upstream releases: v1.37.0 (Xm) and v1.36.4 (Xp).

## Things to know before running it

- **Moving an existing cluster to Kyvernetria restarts workloads.** The pod
  template default for `terminationGracePeriodSeconds` changes from 30 to 300,
  so templates without an explicit value change once. Upstream's own tests warn
  about exactly this.
- **Immunity is strict.** Namespaces running infrastructure (CNI, storage,
  monitoring) need `kubectl label namespace <ns> kyvernetria.io/self=true`.
  `kube-system`, `kube-public`, `kube-node-lease` and `local-path-storage` are
  tolerated by default.
- **Immunity is not a security boundary.** Anyone allowed to label namespaces
  can opt a namespace out, and NoBruteForce is only a speed bump. Keep Pod
  Security Admission and RBAC in place; Immunity does not replace them. More
  security notes are in [docs/OPERATIONS.md](docs/OPERATIONS.md).
- **Upgrades** have their own procedure, described in
  [docs/OPERATIONS.md](docs/OPERATIONS.md).
- **Events live 30 days**, so etcd holds many more of them. Put events in a
  dedicated etcd (`--etcd-servers-overrides=/events#<endpoints>`); see
  [docs/OPERATIONS.md](docs/OPERATIONS.md).
- **Choose subnets that nothing below uses.** On VMs whose resolver is a
  cluster DNS service (inside Cozystack it is 10.96.0.10), a colliding
  `serviceSubnet` lets kube-proxy hijack the nodes' own DNS. The VM config uses
  10.112.0.0/16.

## Not yet implemented

These are mapped in [docs/RESEARCH.md](docs/RESEARCH.md) but not built yet:
different default resource requests (sex differences in pharmacokinetics),
sustained-load tuning (greater fatigue resistance at submaximal effort), deeper
health checks (camouflaging), early adoption of alpha APIs with strict label
conventions (women lead most language change from below), and a longer support window.
Log "sniffing" was dropped: the sense-of-smell advantage is small to trivial.

## License

Apache License 2.0, like Kubernetes. Kyvernetria is not affiliated with the
Kubernetes project or the CNCF. See [NOTICE](NOTICE).
