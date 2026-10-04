/*
Copyright 2026 The Kyvernetria Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kyvctl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"

	"k8s.io/kubernetes/pkg/kyvernetria"
	api "k8s.io/kubernetes/pkg/kyvernetria/gestation"
)

// errNoGestationAPI explains a missing CRD.
var errNoGestationAPI = fmt.Errorf("the Gestation API isn't installed yet. " +
	"kube-controller-manager installs it when its kyvernetria-gestation-controller starts")

func gestations(dyn dynamic.Interface, ns string) dynamic.ResourceInterface {
	return dyn.Resource(api.GVR).Namespace(ns)
}

func getGestation(ctx context.Context, dyn dynamic.Interface, ns, name string) (*api.Gestation, error) {
	u, err := gestations(dyn, ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			if _, listErr := gestations(dyn, ns).List(ctx, metav1.ListOptions{Limit: 1}); apierrors.IsNotFound(listErr) {
				return nil, errNoGestationAPI
			}
			return nil, fmt.Errorf("there is no gestation %s in namespace %s", name, ns)
		}
		return nil, err
	}
	return api.FromUnstructured(u)
}

func clients(f cmdutil.Factory) (string, dynamic.Interface, kubernetes.Interface, error) {
	ns, _, err := f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return "", nil, nil, err
	}
	dyn, err := f.DynamicClient()
	if err != nil {
		return "", nil, nil, err
	}
	kube, err := f.KubernetesClientSet()
	if err != nil {
		return "", nil, nil, err
	}
	return ns, dyn, kube, nil
}

// --- conceive ---------------------------------------------------------

// ConceiveOptions are kyvctl conceive's flags.
type ConceiveOptions struct {
	Due        string
	Size       string // cpu/memory
	Replicas   int32
	Deployment string
	NoRollback bool
}

func newConceiveCommand(f cmdutil.Factory, streams genericiooptions.IOStreams) *cobra.Command {
	o := ConceiveOptions{Replicas: 1}
	c := &cobra.Command{
		Use:   "conceive NAME --due YYYY-MM-DD --size CPU/MEMORY [--replicas N]",
		Short: "Plan a service's launch: reserve room for it ahead of time and screen it before it is born",
		Long: "conceive creates a Gestation. Until the due date the cluster holds room for the service with\n" +
			"low-priority placeholder pods, growing in trimester steps, so launch day never waits for capacity:\n" +
			"any real pod preempts them, and the cluster autoscaler adds nodes for them. Every trimester, and\n" +
			"on demand (kyvctl screen), the service is screened: image, Secrets and ConfigMaps (by name),\n" +
			"volumes, quota, probes, requests and a disruption budget. Deliver it with kyvctl deliver.",
		Example: "  kyvctl conceive web -n shop --due 2026-11-01 --size 500m/512Mi --replicas 3",
		Args:    cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			ns, dyn, _, err := clients(f)
			cmdutil.CheckErr(err)
			g, err := Conceive(cmd.Context(), dyn, ns, args[0], o, time.Now())
			cmdutil.CheckErr(err)
			renderConceived(streams.Out, g, time.Now())
		},
	}
	c.Flags().StringVar(&o.Due, "due", "", "Launch day, YYYY-MM-DD")
	c.Flags().StringVar(&o.Size, "size", "", "What one replica requests, CPU/MEMORY (for example 500m/512Mi)")
	c.Flags().Int32Var(&o.Replicas, "replicas", o.Replicas, "Replicas it launches with")
	c.Flags().StringVar(&o.Deployment, "deployment", "", "The Deployment that will be born (default: NAME)")
	c.Flags().BoolVar(&o.NoRollback, "no-rollback", false, "Don't roll back when the Apgar score at five minutes is below 7")
	return c
}

// Conceive validates the options and creates the Gestation.
func Conceive(ctx context.Context, dyn dynamic.Interface, ns, name string, o ConceiveOptions, now time.Time) (*api.Gestation, error) {
	due, err := time.Parse(time.DateOnly, o.Due)
	if err != nil {
		return nil, fmt.Errorf("--due needs a date like 2026-11-01")
	}
	if !due.After(now) {
		return nil, fmt.Errorf("the due date %s is not in the future", o.Due)
	}
	cpu, mem, ok := strings.Cut(o.Size, "/")
	if !ok {
		return nil, fmt.Errorf("--size needs CPU/MEMORY, for example 500m/512Mi")
	}
	size := api.Size{CPU: strings.TrimSpace(cpu), Memory: strings.TrimSpace(mem)}
	if _, err := api.ParseSize(size); err != nil {
		return nil, fmt.Errorf("--size: %w", err)
	}
	if o.Replicas < 1 {
		return nil, fmt.Errorf("--replicas must be at least 1")
	}
	g := &api.Gestation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       api.Spec{Deployment: o.Deployment, Due: o.Due, Size: size, Replicas: o.Replicas},
	}
	if g.Spec.Deployment == "" {
		g.Spec.Deployment = name
	}
	if o.NoRollback {
		no := false
		g.Spec.AutoRollback = &no
	}
	u, err := api.ToUnstructured(g)
	if err != nil {
		return nil, err
	}
	delete(u.Object, "status")
	created, err := gestations(dyn, ns).Create(ctx, u, metav1.CreateOptions{FieldManager: "kyvctl"})
	switch {
	case apierrors.IsNotFound(err):
		return nil, errNoGestationAPI
	case apierrors.IsAlreadyExists(err):
		return nil, fmt.Errorf("%s/%s is already on its way (kubectl get gestation %s -n %s)", ns, name, name, ns)
	case err != nil:
		return nil, err
	}
	return api.FromUnstructured(created)
}

func renderConceived(out io.Writer, g *api.Gestation, now time.Time) {
	due, _ := g.Spec.DueTime()
	days := int(due.Sub(now).Hours()/24) + 1
	fmt.Fprintf(out, "Conceived %s/%s, due %s (in %d days).\n", g.Namespace, g.Name, g.Spec.Due, days)
	fmt.Fprintf(out, "Room for its %d replicas, and one more for newborn care, is reserved in trimester steps.\n", g.Spec.Replicas)
	fmt.Fprintf(out, "It is screened every trimester; for a look now: kyvctl screen %s -n %s\n", g.Name, g.Namespace)
}

// --- screen -----------------------------------------------------------

func newScreenCommand(f cmdutil.Factory, streams genericiooptions.IOStreams) *cobra.Command {
	var timeout time.Duration
	c := &cobra.Command{
		Use:     "screen NAME",
		Short:   "Run prenatal screening on a planned service now and show what it found",
		Example: "  kyvctl screen web -n shop",
		Args:    cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			ns, dyn, _, err := clients(f)
			cmdutil.CheckErr(err)
			g, err := Screen(cmd.Context(), dyn, ns, args[0], time.Now(), timeout, time.Second)
			cmdutil.CheckErr(err)
			renderScreening(streams.Out, g)
		},
	}
	c.Flags().DurationVar(&timeout, "timeout", 30*time.Second, "How long to wait for the controller's answer")
	return c
}

// Screen asks for a screening and waits for the controller to answer.
func Screen(ctx context.Context, dyn dynamic.Interface, ns, name string, now time.Time, timeout, poll time.Duration) (*api.Gestation, error) {
	if _, err := getGestation(ctx, dyn, ns, name); err != nil {
		return nil, err
	}
	asked := metav1.NewTime(now.Truncate(time.Second))
	patch, _ := json.Marshal(map[string]interface{}{"spec": map[string]interface{}{"screenRequested": asked}})
	if _, err := gestations(dyn, ns).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "kyvctl"}); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		g, err := getGestation(ctx, dyn, ns, name)
		if err != nil {
			return nil, err
		}
		if r := g.Status.ScreenedRequest; r != nil && r.Equal(&asked) {
			return g, nil
		}
		if !time.Now().Before(deadline) {
			return g, fmt.Errorf("the gestation controller hasn't answered within %s; the result will be in kubectl describe gestation %s -n %s", timeout, name, ns)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}
}

func renderScreening(out io.Writer, g *api.Gestation) {
	s := g.Status.Screening
	if s == nil {
		fmt.Fprintf(out, "%s/%s hasn't been screened yet.\n", g.Namespace, g.Name)
		return
	}
	fmt.Fprintf(out, "Prenatal screening of %s/%s (%s, %s)\n\n", g.Namespace, g.Name, s.Reason, s.Time.Local().Format("2006-01-02 15:04"))
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, f := range s.Findings {
		fmt.Fprintf(w, "  %s\t%s\t%s\n", f.Result, f.Check, f.Message)
	}
	w.Flush()
	_, msg := api.Summarize(s.Reason, s.Findings)
	fmt.Fprintf(out, "\n%s\n", msg)
}

// --- deliver ----------------------------------------------------------

func newDeliverCommand(f cmdutil.Factory, streams genericiooptions.IOStreams) *cobra.Command {
	var again bool
	c := &cobra.Command{
		Use:   "deliver NAME [--again]",
		Short: "Launch a planned service: release its reserved room and let it be born",
		Long: "deliver releases the placeholders, then scales the Deployment up (from 0, or unpauses it) with one\n" +
			"extra replica, a PodDisruptionBudget and a priority bump for its first 72 hours. Apply the\n" +
			"Deployment first (paused, or with 0 replicas), or apply it right after: delivery waits for it.\n" +
			"The launch is scored at one and five minutes; see kyvctl apgar.",
		Example: "  kubectl apply -f web.yaml   # paused: true, or replicas: 0\n  kyvctl deliver web -n shop",
		Args:    cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			ns, dyn, _, err := clients(f)
			cmdutil.CheckErr(err)
			g, err := Deliver(cmd.Context(), dyn, ns, args[0], again)
			cmdutil.CheckErr(err)
			fmt.Fprintf(streams.Out, "Delivering %s/%s. Its reserved room is being released for its own pods.\n", g.Namespace, g.Name)
			fmt.Fprintf(streams.Out, "The first Apgar score comes at one minute: kyvctl apgar %s -n %s\n", g.Name, g.Namespace)
		},
	}
	c.Flags().BoolVar(&again, "again", false, "Deliver again after a rollback")
	return c
}

// Deliver asks the controller to deliver.
func Deliver(ctx context.Context, dyn dynamic.Interface, ns, name string, again bool) (*api.Gestation, error) {
	g, err := getGestation(ctx, dyn, ns, name)
	if err != nil {
		return nil, err
	}
	switch {
	case g.Status.Phase == api.PhaseRolledBack && !again:
		return nil, fmt.Errorf("%s went back to revision %s after its Apgar score. When it is ready: kyvctl deliver %s --again",
			g.Spec.Deployment, g.Status.Birth.RolledBackTo, name)
	case g.Status.Phase == api.PhaseDeparted:
		return nil, fmt.Errorf("%s has left the cluster; conceive a new one", g.Spec.Deployment)
	case g.Born() && g.Status.Phase != api.PhaseRolledBack:
		return nil, fmt.Errorf("%s was born on %s. A new rollout is not a new birth", g.Spec.Deployment,
			g.Status.Birth.Time.Local().Format("2006-01-02 15:04"))
	case g.Spec.Delivery > g.Status.Delivery:
		return g, nil // already asked
	}
	next := g.Status.Delivery + 1
	patch, _ := json.Marshal(map[string]interface{}{"spec": map[string]interface{}{"delivery": next}})
	u, err := gestations(dyn, ns).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "kyvctl"})
	if err != nil {
		return nil, err
	}
	return api.FromUnstructured(u)
}

// --- apgar ------------------------------------------------------------

func newApgarCommand(f cmdutil.Factory, streams genericiooptions.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "apgar NAME|deploy/NAME",
		Short: "Show a launch's Apgar scores, sign by sign",
		Long: "The launch is scored like a newborn, five signs from 0 to 2, at one and five minutes, and every\n" +
			"five minutes up to 20 while it stays below 7:\n" +
			"  appearance   ready replicas\n" +
			"  pulse        restarts and liveness failures\n" +
			"  grimace      warning events\n" +
			"  activity     ready endpoints behind its Services (running pods if nothing calls it)\n" +
			"  respiration  OOM kills and evictions\n" +
			"Below 7 at five minutes, the Deployment goes back to its previous revision (unless conceived with --no-rollback).",
		Example: "  kyvctl apgar web -n shop\n  kyvctl apgar deploy/web -n shop",
		Args:    cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			ns, dyn, _, err := clients(f)
			cmdutil.CheckErr(err)
			g, err := findGestation(cmd.Context(), dyn, ns, args[0])
			cmdutil.CheckErr(err)
			renderApgar(streams.Out, g)
		},
	}
}

// findGestation accepts a gestation name or deploy/NAME.
func findGestation(ctx context.Context, dyn dynamic.Interface, ns, target string) (*api.Gestation, error) {
	kind, name, ok := strings.Cut(target, "/")
	if !ok {
		return getGestation(ctx, dyn, ns, target)
	}
	if normalizeKind(kind) != "Deployment" {
		return nil, fmt.Errorf("I can look up a gestation by its name or by deploy/NAME, not by %s", kind)
	}
	list, err := gestations(dyn, ns).List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		return nil, errNoGestationAPI
	}
	if err != nil {
		return nil, err
	}
	var found *api.Gestation
	for i := range list.Items {
		g, err := api.FromUnstructured(&list.Items[i])
		if err != nil || g.Spec.Deployment != name {
			continue
		}
		if found == nil || g.Status.Birth != nil && (found.Status.Birth == nil || found.Status.Birth.Time.Before(&g.Status.Birth.Time)) {
			found = g
		}
	}
	if found == nil {
		return nil, fmt.Errorf("no gestation in namespace %s is for Deployment %s", ns, name)
	}
	return found, nil
}

func renderApgar(out io.Writer, g *api.Gestation) {
	b := g.Status.Birth
	if b == nil {
		fmt.Fprintf(out, "%s/%s isn't born yet (%s). Its Apgar score comes one minute after kyvctl deliver.\n",
			g.Namespace, g.Name, strings.ToLower(orDefault(g.Status.Phase, "expecting")))
		return
	}
	fmt.Fprintf(out, "%s (%s), born %s, revision %s\n", g.Spec.Deployment, g.Namespace,
		b.Time.Local().Format("2006-01-02 15:04"), orDefault(b.Revision, "not known yet"))
	if len(g.Status.Apgar) == 0 {
		fmt.Fprintln(out, "\nNo score yet: the first comes at one minute.")
		return
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprint(w, "\t")
	for _, a := range g.Status.Apgar {
		fmt.Fprintf(w, "%d min\t", a.Minute)
	}
	fmt.Fprintln(w, "\t")
	last := g.Status.Apgar[len(g.Status.Apgar)-1]
	signs := []string{"Appearance", "Pulse", "Grimace", "Activity", "Respiration"}
	value := func(a api.Apgar, i int) int32 {
		return [...]int32{a.Appearance, a.Pulse, a.Grimace, a.Activity, a.Respiration}[i]
	}
	for i, sign := range signs {
		fmt.Fprintf(w, "%s\t", sign)
		for _, a := range g.Status.Apgar {
			fmt.Fprintf(w, "%d\t", value(a, i))
		}
		note := ""
		if i < len(last.Notes) {
			note = last.Notes[i]
		}
		fmt.Fprintf(w, "   %s\t\n", note)
	}
	fmt.Fprint(w, "Total\t")
	for _, a := range g.Status.Apgar {
		fmt.Fprintf(w, "%d\t", a.Total)
	}
	fmt.Fprintln(w, "\t")
	w.Flush()
	switch {
	case b.RolledBackTo != "":
		fmt.Fprintf(out, "\nBelow 7 at five minutes: it went back to revision %s, where it was well.\n", b.RolledBackTo)
	case last.Minute >= 5 && last.Total >= api.Reassuring:
		fmt.Fprintf(out, "\n%s arrived. Apgar %d at %s. Welcome.\n", g.Spec.Deployment, last.Total, api.MinuteWords(last.Minute))
	case last.Total < api.Reassuring:
		fmt.Fprintf(out, "\nBelow 7 at %s. It needs looking after.\n", api.MinuteWords(last.Minute))
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// --- growth -----------------------------------------------------------

func newGrowthCommand(f cmdutil.Factory, streams genericiooptions.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "growth NAME",
		Short: "Chart a service's use against its own trajectory and its requests",
		Long: "growth draws percentile bands (3rd, 15th, 50th, 85th, 97th, as WHO growth charts do) from the\n" +
			"service's own history, measured by the gestation controller through the metrics API, and\n" +
			"compares the latest use with them and with what the service requests. Without metrics-server\n" +
			"there is nothing to chart, and it says so.",
		Example: "  kyvctl growth web -n shop",
		Args:    cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			ns, dyn, kube, err := clients(f)
			cmdutil.CheckErr(err)
			g, err := getGestation(cmd.Context(), dyn, ns, args[0])
			cmdutil.CheckErr(err)
			var metrics metricsclient.Interface
			if cfg, err := f.ToRESTConfig(); err == nil {
				metrics, _ = metricsclient.NewForConfig(cfg)
			}
			cmdutil.CheckErr(Growth(cmd.Context(), kube, metrics, g, streams.Out))
		},
	}
}

// Growth renders a service's growth chart.
func Growth(ctx context.Context, kube kubernetes.Interface, metrics metricsclient.Interface, g *api.Gestation, out io.Writer) error {
	if g.Status.Birth == nil {
		fmt.Fprintf(out, "%s/%s isn't born yet, so it hasn't grown.\n", g.Namespace, g.Name)
		return nil
	}
	var reqCPU, reqMem int64
	d, err := kube.AppsV1().Deployments(g.Namespace).Get(ctx, g.Spec.Deployment, metav1.GetOptions{})
	if err == nil {
		for _, c := range d.Spec.Template.Spec.Containers {
			reqCPU += c.Resources.Requests.Cpu().MilliValue()
			reqMem += c.Resources.Requests.Memory().Value()
		}
	}
	var live *api.Sample
	if metrics != nil && g.Status.Birth.Selector != "" {
		if list, err := metrics.MetricsV1beta1().PodMetricses(g.Namespace).List(ctx, metav1.ListOptions{LabelSelector: g.Status.Birth.Selector}); err == nil && len(list.Items) > 0 {
			var cpu, mem int64
			for _, pm := range list.Items {
				for _, c := range pm.Containers {
					cpu += c.Usage.Cpu().MilliValue()
					mem += c.Usage.Memory().Value()
				}
			}
			n := int64(len(list.Items))
			live = &api.Sample{CPUMilli: cpu / n, MemoryBytes: mem / n, Pods: int32(n)}
		}
	}
	if len(g.Status.Growth) == 0 && live == nil {
		note := g.Status.GrowthNote
		if note == "" {
			note = "No measurements yet; the first comes within ten minutes of birth."
		}
		fmt.Fprintf(out, "Growth of %s (%s)\n\n%s\n", g.Spec.Deployment, g.Namespace, note)
		return nil
	}
	renderGrowth(out, g, api.AssessGrowth(g.Status.Growth, reqCPU, reqMem, live), live != nil)
	return nil
}

func renderGrowth(out io.Writer, g *api.Gestation, charts []api.Assessment, live bool) {
	span := ""
	if s := g.Status.Growth; len(s) > 1 {
		span = " over " + humanSpan(s[len(s)-1].Time.Sub(s[0].Time.Time))
	}
	fmt.Fprintf(out, "Growth of %s (%s): %d measurements%s, per pod\n\n", g.Spec.Deployment, g.Namespace, len(g.Status.Growth), span)
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', tabwriter.AlignRight)
	now := "latest"
	if live {
		now = "now"
	}
	fmt.Fprintf(w, "\t%s\tP3\tP15\tP50\tP85\tP97\trequest\t\n", now)
	for _, a := range charts {
		f := formatCPU
		name := "CPU"
		if a.Resource == "memory" {
			f, name = formatMemory, "Memory"
		}
		req := "none"
		if a.Request > 0 {
			req = f(a.Request)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t\n", name, f(a.Current),
			f(a.Bands.P3), f(a.Bands.P15), f(a.Bands.P50), f(a.Bands.P85), f(a.Bands.P97), req)
	}
	w.Flush()
	fmt.Fprintln(out)
	for _, a := range charts {
		name := "CPU"
		if a.Resource == "memory" {
			name = "Memory"
		}
		fmt.Fprintf(out, "%s: %s.\n", name, a.Hint)
	}
}

func formatCPU(m int64) string { return resource.NewMilliQuantity(m, resource.DecimalSI).String() }

func formatMemory(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fGi", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%dMi", b>>20)
	}
	return fmt.Sprintf("%dKi", b>>10)
}

func humanSpan(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	}
	return fmt.Sprintf("%d minutes", int(d.Minutes()))
}

// --- remember (the memory of services that left) ----------------------

// Memory reads the records the cluster keeps of the services it gave
// birth to.
func Memory(ctx context.Context, kube kubernetes.Interface) ([]api.Record, error) {
	cm, err := kube.CoreV1().ConfigMaps(kyvernetria.SystemNamespace).Get(ctx, api.MemoryConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return api.Records(cm.Data), nil
}

func renderMemory(out io.Writer, records []api.Record, all bool) {
	var shown []api.Record
	for _, r := range records {
		if all || r.Departed() {
			shown = append(shown, r)
		}
	}
	if len(shown) == 0 {
		if all {
			fmt.Fprintln(out, "No service has been born here yet.")
		} else {
			fmt.Fprintln(out, "No service born here has left yet. (kyvctl remember --all lists the ones still here.)")
		}
		return
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SERVICE\tBORN\tLEFT\tCAREGIVERS\tDEPENDED ON\tLAST CONFIG")
	for _, r := range shown {
		left := "still here"
		if r.Departed() {
			left = shortDate(r.Left)
		}
		fmt.Fprintf(w, "%s/%s\t%s\t%s\t%s\t%s\t%s\n", r.Namespace, r.Name, shortDate(r.Born), left,
			orDefault(strings.Join(r.Owners, ", "), "-"), orDefault(strings.Join(r.Dependencies, ", "), "-"), orDefault(r.ConfigDigest, "-"))
	}
	w.Flush()
	fmt.Fprintf(out, "\n%s\n", api.Remembrance)
}

func shortDate(stamp string) string {
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return stamp
	}
	return t.Local().Format(time.DateOnly)
}
