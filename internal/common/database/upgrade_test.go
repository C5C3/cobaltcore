// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package database

import (
	"context"
	"errors"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/job"
	"github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// upgradeOwner is an owner CR named after flowInstance so the phase Jobs the
// flow builds ("<instance>-db-expand") and the Jobs AbortUpgrade deletes
// ("<owner name>-db-expand") share one name, matching the real controller where
// the owner CR name IS the JobSetParams instance name.
func upgradeOwner() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: flowInstance, Namespace: flowNamespace, UID: "upgrade-uid"},
	}
}

// phaseJobBuilder returns a BuildPhaseJob closure that builds the expand/migrate/
// contract Jobs for the keystone-shaped JobSetParams, mirroring how a caller
// pins the phase command and target-release image per phase.
func phaseJobBuilder(p JobSetParams) func(commonv1.UpgradePhase) *batchv1.Job {
	return func(phase commonv1.UpgradePhase) *batchv1.Job {
		switch phase {
		case commonv1.UpgradePhaseExpanding:
			return BuildJob(p, p.Image, "db-expand", []string{"keystone-manage", "db_sync", "--expand"}, 4)
		case commonv1.UpgradePhaseMigrating:
			return BuildJob(p, p.Image, "db-migrate", []string{"keystone-manage", "db_sync", "--migrate"}, 4)
		case commonv1.UpgradePhaseContracting:
			return BuildJob(p, p.Image, "db-contract", []string{"keystone-manage", "db_sync", "--contract"}, 4)
		case commonv1.UpgradePhaseRollingUpdate:
			// RollingUpdate runs no Job; the flow never calls BuildPhaseJob for it.
			return nil
		default:
			return nil
		}
	}
}

// completedPhaseJob seeds a finished phase Job whose stored re-run key matches
// the desired template, so RunJob observes it as done rather than re-creating it.
func completedPhaseJob(builder func(commonv1.UpgradePhase) *batchv1.Job, phase commonv1.UpgradePhase) *batchv1.Job {
	pj := builder(phase)
	return completedJob(pj.Name, job.PodSpecHash(&pj.Spec.Template))
}

// runningPhaseJob seeds a phase Job that exists but carries no terminal
// condition, so RunJob reports it as still in progress.
func runningPhaseJob(builder func(commonv1.UpgradePhase) *batchv1.Job, phase commonv1.UpgradePhase) *batchv1.Job {
	pj := builder(phase)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: pj.Name, Namespace: flowNamespace,
			Annotations: map[string]string{job.PodSpecHashAnnotation: job.PodSpecHash(&pj.Spec.Template)},
		},
	}
}

// failedPhaseJob seeds a permanently failed phase Job whose re-run key matches
// the desired template, so RunJob returns ErrJobFailed rather than re-creating it.
func failedPhaseJob(builder func(commonv1.UpgradePhase) *batchv1.Job, phase commonv1.UpgradePhase) *batchv1.Job {
	pj := builder(phase)
	return failedJob(pj.Name, job.PodSpecHash(&pj.Spec.Template))
}

// upgradeState holds the three mutable status fields the flow advances behind
// pointers, so a test can seed them and assert their final values.
type upgradeState struct {
	phase     commonv1.UpgradePhase
	installed string
	target    string
}

// upgradeParams assembles a keystone-shaped UpgradeFlowParams over st.
func upgradeParams(c client.Client, s *runtime.Scheme, owner client.Object, rec record.EventRecorder, conds *[]metav1.Condition, st *upgradeState, spec string) UpgradeFlowParams {
	return UpgradeFlowParams{
		Client:           c,
		Scheme:           s,
		Recorder:         rec,
		Owner:            owner,
		Conditions:       conds,
		Generation:       1,
		ConditionType:    "DatabaseReady",
		RequeueAfter:     30 * time.Second,
		Phase:            &st.phase,
		InstalledRelease: &st.installed,
		TargetRelease:    &st.target,
		SpecRelease:      spec,
		BuildPhaseJob:    phaseJobBuilder(keystoneJobSet()),
	}
}

// --- IsUpgrade ---

func TestIsUpgrade(t *testing.T) {
	g := NewWithT(t)
	cases := []struct {
		name      string
		installed string
		spec      string
		want      bool
	}{
		{"empty installed release is a fresh deploy", "", "2026.1", false},
		{"same release is no upgrade", "2025.2", "2025.2", false},
		{"empty spec release (digest-pinned) is no upgrade", "2025.2", "", false},
		{"patch-only bump is no upgrade", "2025.2", "2025.2-p1", false},
		{"sequential bump is an upgrade", "2025.2", "2026.1", true},
		{"skip-level bump is an upgrade", "2025.1", "2026.1", true},
		{"unparseable installed release defers to InitiateUpgrade", "latest", "2026.1", true},
		{"unparseable spec release defers to InitiateUpgrade", "2025.2", "latest", true},
	}
	for _, tc := range cases {
		g.Expect(IsUpgrade(tc.spec, tc.installed)).To(Equal(tc.want), tc.name)
	}
}

// --- InitiateUpgrade ---

func TestInitiateUpgrade_HappyPath(t *testing.T) {
	g := NewWithT(t)
	var conds []metav1.Condition
	rec := record.NewFakeRecorder(10)
	st := upgradeState{phase: "", installed: "2025.2", target: ""}
	p := upgradeParams(nil, nil, upgradeOwner(), rec, &conds, &st, "2026.1")

	res, err := InitiateUpgrade(context.Background(), p)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: reconcile.RequeueNextPass}))
	// Target and phase are stamped for ReconcileUpgrade to pick up next pass.
	g.Expect(st.target).To(Equal("2026.1"))
	g.Expect(st.phase).To(Equal(commonv1.UpgradePhaseExpanding))
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(ReasonExpandInProgress))
	g.Expect(rec.Events).To(Receive(ContainSubstring(ReasonUpgradeInitiated)))
}

func TestInitiateUpgrade_VersionParseErrorInstalled(t *testing.T) {
	g := NewWithT(t)
	var conds []metav1.Condition
	rec := record.NewFakeRecorder(10)
	st := upgradeState{phase: "", installed: "latest", target: ""}
	p := upgradeParams(nil, nil, upgradeOwner(), rec, &conds, &st, "2026.1")

	res, err := InitiateUpgrade(context.Background(), p)
	g.Expect(err).To(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(ReasonVersionParseError))
	g.Expect(cond.Message).To(ContainSubstring("installed release"))
	// The upgrade state is untouched on a validation failure.
	g.Expect(st.phase).To(Equal(commonv1.UpgradePhase("")))
	g.Expect(st.target).To(BeEmpty())
	g.Expect(rec.Events).To(Receive(ContainSubstring(ReasonVersionParseError)))
}

func TestInitiateUpgrade_VersionParseErrorSpec(t *testing.T) {
	g := NewWithT(t)
	var conds []metav1.Condition
	rec := record.NewFakeRecorder(10)
	st := upgradeState{phase: "", installed: "2025.2", target: ""}
	p := upgradeParams(nil, nil, upgradeOwner(), rec, &conds, &st, "latest")

	res, err := InitiateUpgrade(context.Background(), p)
	g.Expect(err).To(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(ReasonVersionParseError))
	g.Expect(cond.Message).To(ContainSubstring("target release"))
	g.Expect(st.phase).To(Equal(commonv1.UpgradePhase("")))
	g.Expect(st.target).To(BeEmpty())
	g.Expect(rec.Events).To(Receive(ContainSubstring(ReasonVersionParseError)))
}

func TestInitiateUpgrade_Downgrade(t *testing.T) {
	g := NewWithT(t)
	var conds []metav1.Condition
	rec := record.NewFakeRecorder(10)
	st := upgradeState{phase: "", installed: "2026.1", target: ""}
	p := upgradeParams(nil, nil, upgradeOwner(), rec, &conds, &st, "2025.2")

	res, err := InitiateUpgrade(context.Background(), p)
	g.Expect(err).To(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(ReasonDowngradeNotSupported))
	g.Expect(st.phase).To(Equal(commonv1.UpgradePhase("")))
	g.Expect(st.target).To(BeEmpty())
	g.Expect(rec.Events).To(Receive(ContainSubstring(ReasonDowngradeNotSupported)))
}

func TestInitiateUpgrade_NonSequential(t *testing.T) {
	g := NewWithT(t)
	var conds []metav1.Condition
	rec := record.NewFakeRecorder(10)
	// 2025.1 -> 2026.1 skips 2025.2, so it is a forward but non-sequential jump.
	st := upgradeState{phase: "", installed: "2025.1", target: ""}
	p := upgradeParams(nil, nil, upgradeOwner(), rec, &conds, &st, "2026.1")

	res, err := InitiateUpgrade(context.Background(), p)
	g.Expect(err).To(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(ReasonUpgradePathInvalid))
	g.Expect(st.phase).To(Equal(commonv1.UpgradePhase("")))
	g.Expect(st.target).To(BeEmpty())
	g.Expect(rec.Events).To(Receive(ContainSubstring(ReasonUpgradePathInvalid)))
}

// --- ReconcileUpgrade phase transitions ---

func TestReconcileUpgrade_ExpandCompleteTransitionsToMigrating(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := upgradeOwner()
	var conds []metav1.Condition
	rec := record.NewFakeRecorder(10)
	builder := phaseJobBuilder(keystoneJobSet())
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, completedPhaseJob(builder, commonv1.UpgradePhaseExpanding)).
		Build()

	st := upgradeState{phase: commonv1.UpgradePhaseExpanding, installed: "2025.2", target: "2026.1"}
	res, err := ReconcileUpgrade(context.Background(), upgradeParams(c, s, owner, rec, &conds, &st, "2026.1"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: reconcile.RequeueNextPass}))
	g.Expect(st.phase).To(Equal(commonv1.UpgradePhaseMigrating))
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(ReasonMigrateInProgress))
	g.Expect(rec.Events).To(Receive(ContainSubstring(ReasonExpandComplete)))
}

func TestReconcileUpgrade_MigrateCompleteTransitionsToRollingUpdate(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := upgradeOwner()
	var conds []metav1.Condition
	rec := record.NewFakeRecorder(10)
	builder := phaseJobBuilder(keystoneJobSet())
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, completedPhaseJob(builder, commonv1.UpgradePhaseMigrating)).
		Build()

	st := upgradeState{phase: commonv1.UpgradePhaseMigrating, installed: "2025.2", target: "2026.1"}
	res, err := ReconcileUpgrade(context.Background(), upgradeParams(c, s, owner, rec, &conds, &st, "2026.1"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: reconcile.RequeueNextPass}))
	g.Expect(st.phase).To(Equal(commonv1.UpgradePhaseRollingUpdate))
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(ReasonUpgradeRollingUpdate))
	g.Expect(rec.Events).To(Receive(ContainSubstring(ReasonMigrateComplete)))
}

func TestReconcileUpgrade_RollingUpdatePassThrough(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := upgradeOwner()
	var conds []metav1.Condition
	rec := record.NewFakeRecorder(10)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()

	st := upgradeState{phase: commonv1.UpgradePhaseRollingUpdate, installed: "2025.2", target: "2026.1"}
	res, err := ReconcileUpgrade(context.Background(), upgradeParams(c, s, owner, rec, &conds, &st, "2026.1"))
	g.Expect(err).NotTo(HaveOccurred())
	// RollingUpdate is a pass-through: zero result, phase unchanged, no Job run.
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(st.phase).To(Equal(commonv1.UpgradePhaseRollingUpdate))
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(ReasonUpgradeRollingUpdate))
}

func TestReconcileUpgrade_ContractCompleteFinalizes(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := upgradeOwner()
	var conds []metav1.Condition
	rec := record.NewFakeRecorder(10)
	builder := phaseJobBuilder(keystoneJobSet())
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, completedPhaseJob(builder, commonv1.UpgradePhaseContracting)).
		Build()

	st := upgradeState{phase: commonv1.UpgradePhaseContracting, installed: "2025.2", target: "2026.1"}
	res, err := ReconcileUpgrade(context.Background(), upgradeParams(c, s, owner, rec, &conds, &st, "2026.1"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	// Target is promoted to installed and the upgrade state is cleared.
	g.Expect(st.installed).To(Equal("2026.1"))
	g.Expect(st.target).To(BeEmpty())
	g.Expect(st.phase).To(Equal(commonv1.UpgradePhase("")))
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(ReasonDatabaseSynced))
	g.Expect(rec.Events).To(Receive(ContainSubstring(ReasonUpgradeComplete)))
}

func TestReconcileUpgrade_InProgressRequeues(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := upgradeOwner()
	var conds []metav1.Condition
	rec := record.NewFakeRecorder(10)
	builder := phaseJobBuilder(keystoneJobSet())
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, runningPhaseJob(builder, commonv1.UpgradePhaseExpanding)).
		Build()

	st := upgradeState{phase: commonv1.UpgradePhaseExpanding, installed: "2025.2", target: "2026.1"}
	res, err := ReconcileUpgrade(context.Background(), upgradeParams(c, s, owner, rec, &conds, &st, "2026.1"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(30 * time.Second))
	g.Expect(st.phase).To(Equal(commonv1.UpgradePhaseExpanding))
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal("ExpandInProgress"))
}

func TestReconcileUpgrade_ExpandJobFailed(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := upgradeOwner()
	var conds []metav1.Condition
	rec := record.NewFakeRecorder(10)
	builder := phaseJobBuilder(keystoneJobSet())
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, failedPhaseJob(builder, commonv1.UpgradePhaseExpanding)).
		Build()

	st := upgradeState{phase: commonv1.UpgradePhaseExpanding, installed: "2025.2", target: "2026.1"}
	res, err := ReconcileUpgrade(context.Background(), upgradeParams(c, s, owner, rec, &conds, &st, "2026.1"))
	g.Expect(err).To(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal("ExpandFailed"))
	g.Expect(rec.Events).To(Receive(ContainSubstring(ReasonExpandFailed)))
}

func TestReconcileUpgrade_RecordTerminalSuffixes(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	builder := phaseJobBuilder(keystoneJobSet())
	cases := []struct {
		phase  commonv1.UpgradePhase
		suffix string
	}{
		{commonv1.UpgradePhaseExpanding, "db-expand"},
		{commonv1.UpgradePhaseMigrating, "db-migrate"},
		{commonv1.UpgradePhaseContracting, "db-contract"},
	}
	for _, tc := range cases {
		owner := upgradeOwner()
		var conds []metav1.Condition
		rec := record.NewFakeRecorder(10)
		var calls []string
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(owner, completedPhaseJob(builder, tc.phase)).
			Build()

		st := upgradeState{phase: tc.phase, installed: "2025.2", target: "2026.1"}
		p := upgradeParams(c, s, owner, rec, &conds, &st, "2026.1")
		p.RecordTerminal = func(jobSuffix string, _ *batchv1.Job) {
			calls = append(calls, jobSuffix)
		}
		_, err := ReconcileUpgrade(context.Background(), p)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(calls).To(Equal([]string{tc.suffix}), string(tc.phase))
	}

	// A nil RecordTerminal must not panic on the same completed-Job path.
	owner := upgradeOwner()
	var conds []metav1.Condition
	rec := record.NewFakeRecorder(10)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, completedPhaseJob(builder, commonv1.UpgradePhaseExpanding)).
		Build()
	st := upgradeState{phase: commonv1.UpgradePhaseExpanding, installed: "2025.2", target: "2026.1"}
	p := upgradeParams(c, s, owner, rec, &conds, &st, "2026.1")
	p.RecordTerminal = nil
	g.Expect(func() {
		_, _ = ReconcileUpgrade(context.Background(), p)
	}).NotTo(Panic())
}

// --- Abort ---

func TestReconcileUpgrade_AbortOnSpecRevert(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := upgradeOwner()
	var conds []metav1.Condition
	rec := record.NewFakeRecorder(10)
	// Seed all three phase Jobs so the abort has something to delete.
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(
			owner,
			phaseJobStub("db-expand"),
			phaseJobStub("db-migrate"),
			phaseJobStub("db-contract"),
		).Build()

	// SpecRelease reverted to the installed release triggers the abort.
	st := upgradeState{phase: commonv1.UpgradePhaseExpanding, installed: "2025.2", target: "2026.1"}
	res, err := ReconcileUpgrade(context.Background(), upgradeParams(c, s, owner, rec, &conds, &st, "2025.2"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: reconcile.RequeueNextPass}))
	g.Expect(st.phase).To(Equal(commonv1.UpgradePhase("")))
	g.Expect(st.target).To(BeEmpty())

	// All three phase Jobs were deleted.
	for _, suffix := range []string{"db-expand", "db-migrate", "db-contract"} {
		j := &batchv1.Job{}
		err := c.Get(context.Background(), client.ObjectKey{Name: flowInstance + "-" + suffix, Namespace: flowNamespace}, j)
		g.Expect(err).To(HaveOccurred(), suffix)
	}
	g.Expect(rec.Events).To(Receive(ContainSubstring(ReasonUpgradeAborted)))
}

func TestAbortUpgrade_DeleteErrorKeepsState(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := upgradeOwner()
	var conds []metav1.Condition
	rec := record.NewFakeRecorder(10)
	boom := errors.New("apiserver unavailable")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.DeleteOption) error {
				return boom
			},
		}).Build()

	st := upgradeState{phase: commonv1.UpgradePhaseExpanding, installed: "2025.2", target: "2026.1"}
	res, err := AbortUpgrade(context.Background(), upgradeParams(c, s, owner, rec, &conds, &st, "2025.2"))
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("deleting upgrade jobs during abort"))
	g.Expect(res.IsZero()).To(BeTrue())
	// The delete error leaves the upgrade state intact so the next pass retries.
	g.Expect(st.phase).To(Equal(commonv1.UpgradePhaseExpanding))
	g.Expect(st.target).To(Equal("2026.1"))
}

// --- Guards ---

func TestReconcileUpgrade_TargetChanged(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := upgradeOwner()
	var conds []metav1.Condition
	rec := record.NewFakeRecorder(10)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()

	// A third spec release, distinct from both installed and the active target.
	st := upgradeState{phase: commonv1.UpgradePhaseExpanding, installed: "2025.2", target: "2026.1"}
	res, err := ReconcileUpgrade(context.Background(), upgradeParams(c, s, owner, rec, &conds, &st, "2026.2"))
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec release changed during active upgrade"))
	g.Expect(res.IsZero()).To(BeTrue())
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(ReasonUpgradeTargetChanged))
	// The guard is inert: it changes no upgrade state.
	g.Expect(st.phase).To(Equal(commonv1.UpgradePhaseExpanding))
	g.Expect(st.target).To(Equal("2026.1"))
	g.Expect(st.installed).To(Equal("2025.2"))
	g.Expect(rec.Events).To(Receive(ContainSubstring(ReasonUpgradeTargetChanged)))
}

func TestReconcileUpgrade_UnknownPhase(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := upgradeOwner()
	var conds []metav1.Condition
	rec := record.NewFakeRecorder(10)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()

	st := upgradeState{phase: commonv1.UpgradePhase("Bogus"), installed: "2025.2", target: "2026.1"}
	_, err := ReconcileUpgrade(context.Background(), upgradeParams(c, s, owner, rec, &conds, &st, "2026.1"))
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(`unknown upgrade phase "Bogus"`))
}

// --- CompleteRollingUpdate ---

func TestCompleteRollingUpdate(t *testing.T) {
	g := NewWithT(t)

	// Every phase but RollingUpdate is a no-op that emits no event.
	for _, phase := range []commonv1.UpgradePhase{
		"",
		commonv1.UpgradePhaseExpanding,
		commonv1.UpgradePhaseMigrating,
		commonv1.UpgradePhaseContracting,
	} {
		var conds []metav1.Condition
		rec := record.NewFakeRecorder(10)
		st := upgradeState{phase: phase, installed: "2025.2", target: "2026.1"}
		p := upgradeParams(nil, nil, upgradeOwner(), rec, &conds, &st, "2026.1")
		g.Expect(CompleteRollingUpdate(context.Background(), p)).To(BeFalse(), string(phase))
		g.Expect(st.phase).To(Equal(phase), string(phase))
		g.Expect(rec.Events).NotTo(Receive())
	}

	// RollingUpdate advances to Contracting and emits DeploymentRolloutComplete.
	var conds []metav1.Condition
	rec := record.NewFakeRecorder(10)
	st := upgradeState{phase: commonv1.UpgradePhaseRollingUpdate, installed: "2025.2", target: "2026.1"}
	p := upgradeParams(nil, nil, upgradeOwner(), rec, &conds, &st, "2026.1")
	g.Expect(CompleteRollingUpdate(context.Background(), p)).To(BeTrue())
	g.Expect(st.phase).To(Equal(commonv1.UpgradePhaseContracting))
	g.Expect(rec.Events).To(Receive(ContainSubstring(ReasonDeploymentRolloutComplete)))
}

// phaseJobStub builds a bare phase Job named "<instance>-<suffix>" for the abort
// tests, which only need the Jobs to exist so the delete has a target.
func phaseJobStub(suffix string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: flowInstance + "-" + suffix, Namespace: flowNamespace},
	}
}
