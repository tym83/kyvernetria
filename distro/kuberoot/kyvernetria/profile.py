"""Turns kuberoot's edge profile into Kyvernetria's: kube-apiserver comes in
two alleles, Xm and Xp, as kuberoot service alternatives. Each node's kinit
silences one at random on its first boot and keeps the choice; an allele
that keeps crashing is escaped to the other. The Xm apiserver emulates the
Xp minor. The controller manager, the scheduler and the node components are
Xp builds in the image itself (extras.sh), so they need no change here.

Usage: profile.py EDGE_PROFILE XP_MINOR > PROFILE"""
import re
import sys

text = open(sys.argv[1]).read()
xp_minor = sys.argv[2]
if not re.fullmatch(r"\d+\.\d+", xp_minor):
    sys.exit(f"Xp minor {xp_minor!r} is not major.minor")

if "\n  name: edge\n" not in text:
    sys.exit("profile of edge changed: no metadata.name to replace")
text = text.replace("\n  name: edge\n", "\n  name: kyvernetria\n", 1)

old = """      - name: kube-apiserver
        args:
        - /usr/bin/kube-apiserver
"""
if text.count(old) != 1:
    sys.exit("profile of edge changed: kube-apiserver is not as expected")
# The escape (docs/OPERATIONS.md, "On kuberoot"): 5 crashes within
# 10 minutes, at most once per boot and once in 24 hours, and never while
# the booted slot is on probation.
new = f"""      - name: kube-apiserver
        alternatives:
          choices:
          - name: Xm
            program: /usr/bin/kube-apiserver.Xm
            args: [--emulated-version={xp_minor}]
          - name: Xp
            program: /usr/bin/kube-apiserver.Xp
          switch: {{crashes: 5, within: 10m, cooldown: 24h}}
        args:
        - /usr/bin/kube-apiserver
        # Mixed-version peer proxy: a request for a resource this apiserver
        # does not serve goes to a peer that does. Peers' serving
        # certificates are signed by the cluster authority.
        - --peer-ca-file={{{{pki "ca.crt"}}}}
"""
text = text.replace(old, new, 1)
sys.stdout.write(text)
