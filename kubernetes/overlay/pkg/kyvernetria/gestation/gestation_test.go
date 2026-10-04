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

package gestation

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"k8s.io/kubernetes/pkg/kyvernetria"
)

var t0 = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func TestTrimester(t *testing.T) {
	due := t0.Add(90 * 24 * time.Hour)
	for _, tc := range []struct {
		at   time.Duration
		want int32
	}{
		{0, 1}, {29 * 24 * time.Hour, 1}, {30 * 24 * time.Hour, 2}, {59 * 24 * time.Hour, 2},
		{60 * 24 * time.Hour, 3}, {89 * 24 * time.Hour, 3}, {90 * 24 * time.Hour, 0}, {200 * 24 * time.Hour, 0},
		{-time.Hour, 1},
	} {
		if got := Trimester(t0, due, t0.Add(tc.at)); got != tc.want {
			t.Errorf("Trimester at %v = %d, want %d", tc.at, got, tc.want)
		}
	}
	if got := Trimester(due, due.Add(-time.Hour), due.Add(-2*time.Hour)); got != 3 {
		t.Errorf("conceived after the due date: trimester %d, want 3", got)
	}
}

func TestReservedGrowsToTermPlusOne(t *testing.T) {
	for _, tc := range []struct{ replicas, t1, t2, t3 int32 }{
		{1, 1, 2, 2}, {3, 1, 3, 4}, {7, 2, 6, 8}, {11, 3, 8, 12},
	} {
		got := []int32{Reserved(1, tc.replicas), Reserved(2, tc.replicas), Reserved(3, tc.replicas), Reserved(0, tc.replicas)}
		want := []int32{tc.t1, tc.t2, tc.t3, tc.t3}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("replicas %d: reserved %v, want %v", tc.replicas, got, want)
		}
		if !(got[0] <= got[1] && got[1] <= got[2]) {
			t.Errorf("replicas %d: reservation shrinks: %v", tc.replicas, got)
		}
	}
}

func TestPlacentaIsHarmless(t *testing.T) {
	g := &Gestation{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"}, Spec: Spec{Deployment: "web"}}
	size, _ := ParseSize(Size{CPU: "500m", Memory: "512Mi"})
	tmpl := &v1.PodTemplateSpec{Spec: v1.PodSpec{
		NodeSelector: map[string]string{"pool": "web"},
		Tolerations:  []v1.Toleration{{Key: "dedicated", Operator: v1.TolerationOpExists}},
		Affinity: &v1.Affinity{
			NodeAffinity: &v1.NodeAffinity{},
			PodAffinity:  &v1.PodAffinity{},
		},
	}}
	d := Placenta(g, "uid-1", 3, size, tmpl)
	spec := d.Spec.Template.Spec
	if d.Name != "web-placenta" || *d.Spec.Replicas != 3 {
		t.Errorf("placenta %s with %d replicas", d.Name, *d.Spec.Replicas)
	}
	if spec.PriorityClassName != PlacentaPriorityClass || PlacentaPriority >= 0 || PlacentaPriority < -10 {
		t.Errorf("placeholders must sit below ordinary pods and above the autoscaler's cutoff: %s %d", spec.PriorityClassName, PlacentaPriority)
	}
	if !strings.Contains(spec.Containers[0].Image, "@sha256:") {
		t.Error("placenta image is not pinned; Immunity would reject it")
	}
	if spec.Containers[0].Resources.Requests.Cpu().String() != "500m" {
		t.Errorf("placeholder requests %v", spec.Containers[0].Resources.Requests)
	}
	if spec.NodeSelector["pool"] != "web" || len(spec.Tolerations) != 1 || spec.Affinity.PodAffinity != nil || spec.Affinity.NodeAffinity == nil {
		t.Errorf("scheduling constraints not copied as intended: %+v", spec)
	}
	if _, ok := d.Spec.Template.Labels["app"]; ok || d.Spec.Template.Labels[kyvernetria.PlacentaLabel] != "web" {
		t.Errorf("placeholder labels %v", d.Spec.Template.Labels)
	}
	if ref := d.OwnerReferences[0]; ref.Kind != Kind || ref.UID != "uid-1" {
		t.Errorf("owner %+v", ref)
	}
	for _, pc := range PriorityClasses() {
		if pc.PreemptionPolicy == nil || *pc.PreemptionPolicy != v1.PreemptNever {
			t.Errorf("%s may preempt other pods", pc.Name)
		}
	}
}

// fakeLookup answers screening from maps.
type fakeLookup struct {
	secrets, configMaps map[string]bool
	pvcs                map[string]*v1.PersistentVolumeClaim
	classes             map[string]*storagev1.StorageClass
	quotas              []*v1.ResourceQuota
	pdbs                []*policyv1.PodDisruptionBudget
	images              map[string]string
	askedForData        bool
}

func (f *fakeLookup) SecretExists(_ context.Context, ns, name string) (bool, error) {
	return f.secrets[ns+"/"+name], nil
}
func (f *fakeLookup) ConfigMapExists(_ context.Context, ns, name string) (bool, error) {
	return f.configMaps[ns+"/"+name], nil
}
func (f *fakeLookup) PVC(ns, name string) (*v1.PersistentVolumeClaim, error) {
	return f.pvcs[ns+"/"+name], nil
}
func (f *fakeLookup) StorageClass(name string) (*storagev1.StorageClass, error) {
	return f.classes[name], nil
}
func (f *fakeLookup) ResourceQuotas(string) ([]*v1.ResourceQuota, error) { return f.quotas, nil }
func (f *fakeLookup) PDBs(string) ([]*policyv1.PodDisruptionBudget, error) {
	return f.pdbs, nil
}
func (f *fakeLookup) Image(_ context.Context, image string) (string, string) {
	if r, ok := f.images[image]; ok {
		return r, "fake"
	}
	return ResultClear, "fake"
}

func gestation(replicas int32) *Gestation {
	return &Gestation{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"},
		Spec: Spec{Deployment: "web", Due: "2026-12-01", Size: Size{CPU: "500m", Memory: "512Mi"}, Replicas: replicas}}
}

func deployment() *appsv1.Deployment {
	probe := &v1.Probe{ProbeHandler: v1.ProbeHandler{HTTPGet: &v1.HTTPGetAction{Path: "/"}}}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"},
		Spec: appsv1.DeploymentSpec{Template: v1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
			Spec: v1.PodSpec{
				ImagePullSecrets: []v1.LocalObjectReference{{Name: "regcred"}},
				Containers: []v1.Container{{
					Name: "web", Image: "ghcr.io/shop/web@sha256:" + strings.Repeat("a", 64),
					Resources: v1.ResourceRequirements{Requests: v1.ResourceList{
						v1.ResourceCPU: resource.MustParse("500m"), v1.ResourceMemory: resource.MustParse("512Mi"),
					}},
					ReadinessProbe: probe, LivenessProbe: probe,
					Env: []v1.EnvVar{
						{Name: "DB", ValueFrom: &v1.EnvVarSource{SecretKeyRef: &v1.SecretKeySelector{LocalObjectReference: v1.LocalObjectReference{Name: "db"}, Key: "url"}}},
						{Name: "OPT", ValueFrom: &v1.EnvVarSource{SecretKeyRef: &v1.SecretKeySelector{LocalObjectReference: v1.LocalObjectReference{Name: "maybe"}, Key: "x", Optional: ptr.To(true)}}},
					},
					EnvFrom: []v1.EnvFromSource{{ConfigMapRef: &v1.ConfigMapEnvSource{LocalObjectReference: v1.LocalObjectReference{Name: "settings"}}}},
				}},
				Volumes: []v1.Volume{{Name: "data", VolumeSource: v1.VolumeSource{PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}}}},
			},
		}},
	}
}

func healthyLookup() *fakeLookup {
	return &fakeLookup{
		secrets:    map[string]bool{"shop/db": true, "shop/regcred": true},
		configMaps: map[string]bool{"shop/settings": true},
		pvcs: map[string]*v1.PersistentVolumeClaim{"shop/data": {
			Spec: v1.PersistentVolumeClaimSpec{Resources: v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("10Gi")}}},
		}},
		classes: map[string]*storagev1.StorageClass{"": {ObjectMeta: metav1.ObjectMeta{Name: "standard"}}},
		pdbs: []*policyv1.PodDisruptionBudget{{ObjectMeta: metav1.ObjectMeta{Name: "web"},
			Spec: policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}}}},
	}
}

func results(fs []Finding) map[string]string {
	out := map[string]string{}
	for _, f := range fs {
		if prev := out[f.Check]; prev == "" || rank(f.Result) > rank(prev) {
			out[f.Check] = f.Result
		}
	}
	return out
}

func rank(r string) int {
	return map[string]int{ResultClear: 0, ResultInfo: 1, ResultWarn: 2, ResultFail: 3}[r]
}

func TestScreenHealthy(t *testing.T) {
	fs := Screen(context.Background(), Input{Gestation: gestation(2), Deployment: deployment()}, healthyLookup())
	for check, r := range results(fs) {
		if r != ResultClear {
			t.Errorf("%s: %s, want clear (%v)", check, r, fs)
		}
	}
	if warning, msg := Summarize("trimester-2", fs); warning || !strings.Contains(msg, "trimester 2: all clear") {
		t.Errorf("summary %v %q", warning, msg)
	}
}

func TestScreenFindsProblems(t *testing.T) {
	d := deployment()
	d.Spec.Template.Spec.Containers[0].ReadinessProbe = nil
	d.Spec.Template.Spec.Containers[0].Resources.Requests = v1.ResourceList{v1.ResourceCPU: resource.MustParse("2")}
	l := healthyLookup()
	delete(l.secrets, "shop/db")
	l.pdbs = nil
	l.classes = map[string]*storagev1.StorageClass{} // no default class
	l.images = map[string]string{d.Spec.Template.Spec.Containers[0].Image: ResultFail}
	l.quotas = []*v1.ResourceQuota{{
		ObjectMeta: metav1.ObjectMeta{Name: "small"},
		Status: v1.ResourceQuotaStatus{
			Hard: v1.ResourceList{v1.ResourceRequestsCPU: resource.MustParse("2"), v1.ResourcePods: resource.MustParse("10")},
			Used: v1.ResourceList{v1.ResourceRequestsCPU: resource.MustParse("1"), v1.ResourcePods: resource.MustParse("2")},
		},
	}}
	fs := Screen(context.Background(), Input{Gestation: gestation(2), Deployment: d}, l)
	want := map[string]string{
		CheckImage: ResultFail, CheckReferences: ResultFail, CheckVolumes: ResultFail, CheckQuota: ResultFail,
		CheckProbes: ResultWarn, CheckRequests: ResultWarn, CheckReservation: ResultWarn, CheckPDB: ResultInfo,
	}
	got := results(fs)
	for check, r := range want {
		if got[check] != r {
			t.Errorf("%s: %s, want %s", check, got[check], r)
		}
	}
	for _, f := range fs {
		if f.Check == CheckReferences && (!strings.Contains(f.Message, "Secret db") || strings.Contains(f.Message, "maybe")) {
			t.Errorf("references: %q (the optional secret must not count)", f.Message)
		}
		if f.Check == CheckQuota && !strings.Contains(f.Message, "the launch needs 1500m") {
			t.Errorf("quota: %q", f.Message)
		}
	}
	if warning, _ := Summarize("requested", fs); !warning {
		t.Error("a screening with failures is not a warning")
	}
}

func TestScreenQuotaCountsPlacentaAsAvailable(t *testing.T) {
	l := healthyLookup()
	l.quotas = []*v1.ResourceQuota{{
		ObjectMeta: metav1.ObjectMeta{Name: "q"},
		Status: v1.ResourceQuotaStatus{
			Hard: v1.ResourceList{v1.ResourceRequestsCPU: resource.MustParse("2"), v1.ResourcePods: resource.MustParse("4")},
			Used: v1.ResourceList{v1.ResourceRequestsCPU: resource.MustParse("1500m"), v1.ResourcePods: resource.MustParse("3")},
		},
	}}
	placenta := v1.ResourceList{v1.ResourceCPU: resource.MustParse("1500m"), v1.ResourcePods: resource.MustParse("3")}
	fs := Screen(context.Background(), Input{Gestation: gestation(2), Deployment: deployment(), Placenta: placenta}, l)
	if r := results(fs)[CheckQuota]; r != ResultClear {
		t.Errorf("quota taken by the placeholders is given back at birth: %s %v", r, fs)
	}
}

func TestScreenWithoutDeployment(t *testing.T) {
	fs := Screen(context.Background(), Input{Gestation: gestation(1)}, &fakeLookup{})
	if r := results(fs); r[CheckDeployment] != ResultInfo || r[CheckQuota] != ResultClear {
		t.Errorf("before the Deployment exists: %v", fs)
	}
}

func TestReferencesNamesOnly(t *testing.T) {
	d := deployment()
	d.Spec.Template.Spec.Volumes = append(d.Spec.Template.Spec.Volumes, v1.Volume{Name: "p", VolumeSource: v1.VolumeSource{
		Projected: &v1.ProjectedVolumeSource{Sources: []v1.VolumeProjection{
			{Secret: &v1.SecretProjection{LocalObjectReference: v1.LocalObjectReference{Name: "tls"}}},
			{ConfigMap: &v1.ConfigMapProjection{LocalObjectReference: v1.LocalObjectReference{Name: "ca"}}},
		}},
	}})
	s, c := References(&d.Spec.Template)
	if fmt.Sprint(s) != "[db regcred tls]" || fmt.Sprint(c) != "[ca settings]" {
		t.Errorf("secrets %v, configmaps %v", s, c)
	}
}

func TestApgarScoring(t *testing.T) {
	for _, tc := range []struct {
		name  string
		signs Signs
		want  [5]int32
		total int32
	}{
		{"healthy", Signs{Desired: 3, Ready: 3, Running: 3, Services: 1, ReadyEndpoint: 3}, [5]int32{2, 2, 2, 2, 2}, 10},
		{"slow to pink up", Signs{Desired: 4, Ready: 2, Running: 4, Services: 1, ReadyEndpoint: 2, Warnings: 2}, [5]int32{1, 2, 1, 1, 2}, 7},
		{"crash loop", Signs{Desired: 3, Ready: 0, Running: 0, Restarts: 6, CrashLooping: true, Services: 1, Warnings: 9, OOMKills: 3},
			[5]int32{0, 0, 0, 0, 0}, 0},
		{"one restart, one OOM", Signs{Desired: 2, Ready: 2, Running: 2, Restarts: 1, OOMKills: 1, Services: 1, ReadyEndpoint: 2}, [5]int32{2, 1, 2, 2, 1}, 8},
		{"liveness only", Signs{Desired: 1, Ready: 1, Running: 1, LivenessFailures: 2, Services: 1, ReadyEndpoint: 1}, [5]int32{2, 1, 2, 2, 2}, 9},
		{"nobody calls it", Signs{Desired: 2, Ready: 2, Running: 2}, [5]int32{2, 2, 2, 2, 2}, 10},
		{"eviction", Signs{Desired: 2, Ready: 2, Running: 2, Evictions: 1, Services: 1, ReadyEndpoint: 2}, [5]int32{2, 2, 2, 2, 1}, 9},
	} {
		a := Score(5, tc.signs)
		got := [5]int32{a.Appearance, a.Pulse, a.Grimace, a.Activity, a.Respiration}
		if got != tc.want || a.Total != tc.total || len(a.Notes) != 5 {
			t.Errorf("%s: %v total %d notes %d, want %v total %d", tc.name, got, a.Total, len(a.Notes), tc.want, tc.total)
		}
	}
	if a := Score(1, Signs{Desired: 2}); !strings.Contains(a.Notes[3], "no Service selects it") {
		t.Errorf("activity note for a service nobody calls: %q", a.Notes[3])
	}
}

func TestApgarSchedule(t *testing.T) {
	score := func(m, total int32) Apgar { return Apgar{Minute: m, Total: total} }
	for _, tc := range []struct {
		scores []Apgar
		want   int32
	}{
		{nil, 1},
		{[]Apgar{score(1, 3)}, 5},
		{[]Apgar{score(1, 9), score(5, 9)}, 0},
		{[]Apgar{score(1, 3), score(5, 6)}, 10},
		{[]Apgar{score(1, 3), score(5, 6), score(10, 7)}, 0},
		{[]Apgar{score(1, 3), score(5, 6), score(10, 6), score(15, 6)}, 20},
		{[]Apgar{score(1, 3), score(5, 6), score(10, 6), score(15, 6), score(20, 6)}, 0},
	} {
		if got := NextMinute(tc.scores); got != tc.want {
			t.Errorf("after %v: next %d, want %d", tc.scores, got, tc.want)
		}
	}
}

func TestRollbackDecision(t *testing.T) {
	low5 := Apgar{Minute: 5, Total: 6}
	if roll, why := Rollback(low5, true, "3"); !roll || !strings.Contains(why, "revision 3") {
		t.Errorf("low five-minute score: %v %q", roll, why)
	}
	for name, tc := range map[string]struct {
		a       Apgar
		enabled bool
		prev    string
		why     string
	}{
		"one-minute score never rolls back": {Apgar{Minute: 1, Total: 2}, true, "3", ""},
		"reassuring":                        {Apgar{Minute: 5, Total: 7}, true, "3", ""},
		"opted out":                         {low5, false, "3", "automatic rollback is off"},
		"first revision":                    {low5, true, "", "no earlier revision"},
		"ten-minute score":                  {Apgar{Minute: 10, Total: 3}, true, "3", ""},
	} {
		roll, why := Rollback(tc.a, tc.enabled, tc.prev)
		if roll || !strings.Contains(why, tc.why) {
			t.Errorf("%s: rolled back %v (%q)", name, roll, why)
		}
	}
}

func TestCareTaper(t *testing.T) {
	var last int32 = 1 << 30
	for h := 0; h < 72; h++ {
		tier, class := CareTier(time.Duration(h) * time.Hour)
		if tier < 1 || class == "" {
			t.Fatalf("hour %d: no care", h)
		}
		v := careTiers[tier-1].priority
		if v > last {
			t.Errorf("hour %d: priority rose to %d", h, v)
		}
		last = v
	}
	if tier, class := CareTier(72 * time.Hour); tier != 0 || class != "" {
		t.Errorf("care did not end at 72 hours: %d %q", tier, class)
	}
	for i := 1; i < len(careTiers); i++ {
		if careTiers[i].priority*2 != careTiers[i-1].priority {
			t.Errorf("tier %d does not halve the one before", i+1)
		}
		if careTiers[i].priority <= 0 {
			t.Error("a care tier is not above ordinary pods")
		}
	}
	if _, class := LastTierClass(); !IsNewbornClass(class) || IsNewbornClass("system-cluster-critical") {
		t.Error("IsNewbornClass is wrong")
	}
}

func TestCaregivers(t *testing.T) {
	a := func(p, s string) map[string]string {
		return map[string]string{kyvernetria.PrimaryCaregiverAnnotation: p, kyvernetria.SecondaryCaregiverAnnotation: s}
	}
	for _, tc := range []struct {
		ann     map[string]string
		ok      bool
		missing string
	}{
		{nil, false, "nobody is named"},
		{a("alice", ""), false, "secondary"},
		{a("", "bob"), false, "primary"},
		{a("alice", "Alice"), false, "named twice"},
		{a(" alice ", "bob"), true, ""},
	} {
		c, ok, missing := CaregiversOf(tc.ann)
		if ok != tc.ok || !strings.Contains(missing, tc.missing) {
			t.Errorf("%v: ok %v missing %q", tc.ann, ok, missing)
		}
		if ok && c.Primary != "alice" {
			t.Errorf("caregiver not trimmed: %q", c.Primary)
		}
	}
}

func TestNewbornAnnotationRoundTrip(t *testing.T) {
	in := []NewbornEntry{{Selector: "app=web", PriorityClass: "kyvernetria-newborn-2"}, {Selector: "app=api", PriorityClass: "kyvernetria-newborn-1"}}
	raw := EncodeNewborns(in)
	out, err := DecodeNewborns(raw)
	if err != nil || len(out) != 2 || out[0].Selector != "app=api" {
		t.Errorf("round trip: %v %v", out, err)
	}
	if EncodeNewborns(nil) != "" {
		t.Error("no entries must encode as no annotation")
	}
	if _, err := DecodeNewborns("{"); err == nil {
		t.Error("garbage decoded")
	}
}

func samples(cpu ...int64) []Sample {
	var out []Sample
	for i, c := range cpu {
		out = append(out, Sample{Time: metav1.NewTime(t0.Add(time.Duration(i) * time.Hour)), CPUMilli: c, MemoryBytes: 100 << 20})
	}
	return out
}

func TestGrowthBands(t *testing.T) {
	steady := samples(100, 110, 120, 130, 140, 150, 160, 170, 180, 190)
	hint := func(s []Sample, req int64, cur *Sample) string { return AssessGrowth(s, req, 1<<30, cur)[0].Hint }

	a := AssessGrowth(steady, 1000, 1<<30, nil)[0]
	if a.Current != 190 || a.Samples != 9 || a.Bands.P50 != 140 || a.Bands.P3 != 100 || a.Bands.P97 != 180 {
		t.Errorf("bands %+v", a)
	}
	if !strings.Contains(a.Hint, "above its own curve") {
		t.Errorf("growing past its own history: %q", a.Hint)
	}
	if h := hint(steady, 150, nil); !strings.HasPrefix(h, "outgrowing its requests: it uses 126%") {
		t.Errorf("over its request: %q", h)
	}
	if h := hint(samples(900, 920, 910, 905, 915, 930, 920, 910), 1000, nil); !strings.Contains(h, "more than 90%") {
		t.Errorf("close to its request: %q", h)
	}
	if h := hint(steady, 1000, &Sample{CPUMilli: 20}); !strings.Contains(h, "falling behind its own curve — check") {
		t.Errorf("dropping below its own curve: %q", h)
	}
	if h := hint(samples(100, 110, 105, 100, 108, 104, 103), 2000, nil); !strings.Contains(h, "plenty of room") {
		t.Errorf("oversized request: %q", h)
	}
	if h := hint(samples(100, 110, 105, 100, 108, 104, 103), 300, nil); h != "on its curve" {
		t.Errorf("on its curve: %q", h)
	}
	if h := hint(samples(100, 110), 300, nil); !strings.Contains(h, "too early") {
		t.Errorf("two measurements: %q", h)
	}
}

func TestSamplesAreBounded(t *testing.T) {
	var s []Sample
	for i := 0; i < MaxSamples+10; i++ {
		s = AppendSample(s, Sample{CPUMilli: int64(i)})
	}
	if len(s) != MaxSamples || s[0].CPUMilli != 10 {
		t.Errorf("kept %d samples starting at %d", len(s), s[0].CPUMilli)
	}
	now := t0
	if !SampleDue(nil, false, now) {
		t.Error("first sample not due")
	}
	last := []Sample{{Time: metav1.NewTime(now)}}
	if SampleDue(last, true, now.Add(9*time.Minute)) || !SampleDue(last, true, now.Add(10*time.Minute)) ||
		SampleDue(last, false, now.Add(30*time.Minute)) {
		t.Error("sample intervals wrong")
	}
}

func TestMemoryIsBounded(t *testing.T) {
	data := map[string]string{}
	for i := 0; i < MaxRecords; i++ {
		r := Record{Namespace: "ns", Name: fmt.Sprintf("svc-%03d", i), UID: fmt.Sprintf("uid%05d-x", i), Born: Stamp(t0.Add(time.Duration(i) * time.Hour))}
		if i < 10 {
			r.Left = Stamp(t0.Add(time.Duration(1000-i) * time.Hour)) // the first ten left; svc-009 left first
		}
		Put(data, r)
	}
	if len(data) != MaxRecords {
		t.Fatalf("%d records", len(data))
	}
	Put(data, Record{Namespace: "ns", Name: "newcomer", UID: "zzz", Born: Stamp(t0.Add(9999 * time.Hour))})
	if len(data) != MaxRecords {
		t.Fatalf("memory grew past its bound: %d", len(data))
	}
	for _, r := range Records(data) {
		if r.Name == "svc-009" {
			t.Error("the record of the service that left longest ago was kept")
		}
	}
	if Put(data, Record{Namespace: "ns", Name: "newcomer", UID: "zzz", Born: Stamp(t0.Add(9999 * time.Hour))}) {
		t.Error("an unchanged record counted as a change")
	}

	long := strings.Repeat("x", 1000)
	var deps []string
	for i := 0; i < 40; i++ {
		deps = append(deps, fmt.Sprintf("svc/dep-%02d", i), "svc/dep-00")
	}
	b := Record{Namespace: "ns", Name: long, UID: "u", Dependencies: deps, Owners: []string{"", "alice"}}.Bounded()
	if len(b.Name) != maxField || len(b.Dependencies) != maxListed || len(b.Owners) != 1 {
		t.Errorf("record not bounded: name %d deps %d owners %v", len(b.Name), len(b.Dependencies), b.Owners)
	}
}

func TestConfigDigestFollowsTemplate(t *testing.T) {
	d := deployment()
	a := ConfigDigest(&d.Spec.Template)
	d.Spec.Template.Spec.Containers[0].Image = "other"
	if b := ConfigDigest(&d.Spec.Template); a == b || !strings.HasPrefix(a, "sha256:") || len(a) != len("sha256:")+12 {
		t.Errorf("digests %q %q", a, b)
	}
}

func TestUnstructuredRoundTrip(t *testing.T) {
	g := gestation(3)
	g.Status.Apgar = []Apgar{{Minute: 1, Total: 8}}
	u, err := ToUnstructured(g)
	if err != nil {
		t.Fatal(err)
	}
	back, err := FromUnstructured(u)
	if err != nil || back.Spec.Replicas != 3 || back.Status.Apgar[0].Total != 8 || back.Kind != Kind {
		t.Errorf("round trip: %+v %v", back, err)
	}
	if !back.Spec.RollbackEnabled() {
		t.Error("rollback must default to on")
	}
}

func TestCRDHasNoAllCategoryAndHasStatus(t *testing.T) {
	crd := CRD()
	if len(crd.Spec.Names.Categories) != 0 {
		t.Error("gestations are in a category; kubectl delete all would take them")
	}
	if crd.Spec.Versions[0].Subresources == nil || crd.Spec.Versions[0].Subresources.Status == nil {
		t.Error("no status subresource")
	}
}
