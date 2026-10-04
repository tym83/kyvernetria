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

// Package gestation holds the Gestation API and the logic of a new
// service's launch that does not need a cluster: trimesters, prenatal
// screening, the Apgar score, newborn care, growth charts and the memory of
// services that have left. The gestation controller and kyvctl share it.
//
// Pregnancy, birth and newborn care are a metaphor here, and a borrowed
// principle, never a borrowed number: every amount and duration is an
// engineering choice. docs/RESEARCH.md (claims 38 to 44) says what each
// one borrows and from which source.
package gestation

import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Group, version and resource of the Gestation API.
const (
	Group    = "kyvernetria.io"
	Version  = "v1alpha1"
	Resource = "gestations"
	Kind     = "Gestation"
)

// GVR is the Gestation resource.
var GVR = schema.GroupVersionResource{Group: Group, Version: Version, Resource: Resource}

// Phases of a Gestation.
const (
	// PhaseExpecting: before the due date; room is reserved in steps and
	// the service is screened every trimester.
	PhaseExpecting = "Expecting"
	// PhaseDue: the due date has passed and nobody has delivered yet.
	// Nothing happens on its own; due dates are estimates.
	PhaseDue = "Due"
	// PhaseDelivering: the reserved room is being released for the
	// service's own pods.
	PhaseDelivering = "Delivering"
	// PhaseNewbornCare: born; protected for the first 72 hours.
	PhaseNewbornCare = "NewbornCare"
	// PhaseNeedsCaregivers: the care period is over, but nobody has named
	// two people on call yet, so the protection stays.
	PhaseNeedsCaregivers = "NeedsCaregivers"
	// PhaseGrown: discharged from newborn care.
	PhaseGrown = "Grown"
	// PhaseRolledBack: the Apgar score at five minutes was low and the
	// Deployment went back to its previous revision.
	PhaseRolledBack = "RolledBack"
	// PhaseDeparted: the Deployment has been deleted (decommissioned or
	// moved elsewhere). The cluster keeps a little record of it.
	PhaseDeparted = "Departed"
)

// Gestation is a service on its way to launch, and its first days after.
type Gestation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              Spec   `json:"spec"`
	Status            Status `json:"status,omitempty"`
}

// Spec is what the people launching the service decided.
type Spec struct {
	// Deployment is the name of the Deployment that will be born, in the
	// Gestation's namespace.
	Deployment string `json:"deployment"`
	// Due is the planned launch day, YYYY-MM-DD.
	Due string `json:"due"`
	// Size is what one replica will request.
	Size Size `json:"size"`
	// Replicas is how many replicas the service launches with.
	Replicas int32 `json:"replicas"`
	// ScreenRequested asks for prenatal screening now (kyvctl screen).
	ScreenRequested *metav1.Time `json:"screenRequested,omitempty"`
	// Delivery is the number of the delivery asked for: kyvctl deliver
	// raises it, and the controller delivers when it is greater than
	// status.delivery.
	Delivery int32 `json:"delivery,omitempty"`
	// AutoRollback returns the Deployment to its previous revision when
	// the Apgar score at five minutes is below 7. Default true.
	AutoRollback *bool `json:"autoRollback,omitempty"`
}

// Size is one replica's requests.
type Size struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

// Status is what the controller observed and did.
type Status struct {
	Phase   string `json:"phase,omitempty"`
	Message string `json:"message,omitempty"`
	// Trimester is 1, 2 or 3 before the due date, 0 otherwise.
	Trimester int32 `json:"trimester,omitempty"`
	// Reserved is the number of placeholder pods holding room.
	Reserved int32 `json:"reserved,omitempty"`
	// Delivery is the number of the last delivery the controller started,
	// and DeliveryStarted when it started.
	Delivery        int32        `json:"delivery,omitempty"`
	DeliveryStarted *metav1.Time `json:"deliveryStarted,omitempty"`
	Screening       *Screening   `json:"screening,omitempty"`
	// ScreenedTrimester is the last trimester screened, and
	// ScreenedRequest the last spec.screenRequested answered.
	ScreenedTrimester int32        `json:"screenedTrimester,omitempty"`
	ScreenedRequest   *metav1.Time `json:"screenedRequest,omitempty"`
	Birth             *Birth       `json:"birth,omitempty"`
	// Apgar holds the scores taken so far, in order: 1 and 5 minutes, then
	// every 5 minutes up to 20 while the score stays below 7.
	Apgar      []Apgar     `json:"apgar,omitempty"`
	Care       *Care       `json:"care,omitempty"`
	Caregivers *Caregivers `json:"caregivers,omitempty"`
	// Growth is the service's own trajectory: average use per pod.
	Growth []Sample `json:"growth,omitempty"`
	// GrowthNote says why there are no measurements, when there are none.
	GrowthNote string `json:"growthNote,omitempty"`
}

// Screening is one prenatal screening.
type Screening struct {
	Time metav1.Time `json:"time"`
	// Reason is "trimester-N" or "requested".
	Reason   string    `json:"reason"`
	Findings []Finding `json:"findings"`
}

// Finding results.
const (
	ResultClear = "clear"
	ResultInfo  = "info"
	ResultWarn  = "warn"
	ResultFail  = "fail"
)

// Finding is one screening check.
type Finding struct {
	Check   string `json:"check"`
	Result  string `json:"result"`
	Message string `json:"message"`
}

// Birth records the launch.
type Birth struct {
	Time metav1.Time `json:"time"`
	// UID of the Deployment that was born.
	UID string `json:"uid,omitempty"`
	// Revision and PreviousRevision are the Deployment revisions of the
	// birth and of the one before it ("" when there was none).
	Revision         string `json:"revision,omitempty"`
	PreviousRevision string `json:"previousRevision,omitempty"`
	// Selector is the Deployment's pod selector, as a string.
	Selector string `json:"selector,omitempty"`
	// AddedReplica is true when newborn care added a replica.
	AddedReplica bool `json:"addedReplica,omitempty"`
	// PDB is the PodDisruptionBudget newborn care created, if any.
	PDB string `json:"pdb,omitempty"`
	// RolledBackTo is the revision the Deployment returned to.
	RolledBackTo string `json:"rolledBackTo,omitempty"`
}

// Care is the state of newborn care.
type Care struct {
	// Tier is 1, 2 or 3 during the care period, 0 after it.
	Tier          int32        `json:"tier"`
	PriorityClass string       `json:"priorityClass,omitempty"`
	Until         metav1.Time  `json:"until"`
	Discharged    *metav1.Time `json:"discharged,omitempty"`
	LastNag       *metav1.Time `json:"lastNag,omitempty"`
	// Notes say which protections did not apply and why.
	Notes []string `json:"notes,omitempty"`
}

// Caregivers are the two people on call.
type Caregivers struct {
	Primary   string `json:"primary,omitempty"`
	Secondary string `json:"secondary,omitempty"`
}

// Sample is the average use of one pod at a moment.
type Sample struct {
	Time        metav1.Time `json:"time"`
	CPUMilli    int64       `json:"cpuMilli"`
	MemoryBytes int64       `json:"memoryBytes"`
	Pods        int32       `json:"pods"`
}

// RollbackEnabled reports whether a low Apgar score rolls back.
func (s *Spec) RollbackEnabled() bool {
	return s.AutoRollback == nil || *s.AutoRollback
}

// DueTime parses the due date as midnight UTC.
func (s *Spec) DueTime() (time.Time, error) {
	t, err := time.Parse(time.DateOnly, s.Due)
	if err != nil {
		return time.Time{}, fmt.Errorf("due date %q is not YYYY-MM-DD", s.Due)
	}
	return t, nil
}

// Born reports whether the service has been born and is still the one
// that was (not rolled back).
func (g *Gestation) Born() bool {
	return g.Status.Birth != nil && g.Status.Phase != PhaseRolledBack
}

// FromUnstructured converts an object read from the API.
func FromUnstructured(u *unstructured.Unstructured) (*Gestation, error) {
	g := &Gestation{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, g); err != nil {
		return nil, fmt.Errorf("reading gestation %s/%s: %w", u.GetNamespace(), u.GetName(), err)
	}
	return g, nil
}

// ToUnstructured converts a Gestation for the API.
func ToUnstructured(g *Gestation) (*unstructured.Unstructured, error) {
	g.APIVersion = Group + "/" + Version
	g.Kind = Kind
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(g)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: obj}, nil
}
