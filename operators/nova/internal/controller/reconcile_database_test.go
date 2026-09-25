// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/database"
	"github.com/c5c3/cobaltcore/internal/common/job"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// testConfigMapName stands in for the rendered config ConfigMap the config step
// hands the database step.
const testConfigMapName = "nova-config-abc12345"

// The two cell UUIDs the fixtures report. cell0 is the all-zero UUID nova maps
// the holding pen under; cell1 carries a generated one.
const (
	testCell0UUID = "00000000-0000-0000-0000-000000000000"
	testCell1UUID = "2683878f-66d5-4512-bac3-9d70555bdd23"
)

// readyCondition is the Ready=True the mariadb-operator writes onto a
// provisioned object once the SQL statement behind it has run.
func readyCondition() metav1.Condition {
	return metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "Ready",
		LastTransitionTime: metav1.Now(),
	}
}

// readyMariaDBCluster returns the MariaDB cluster both blocks of the fixture
// point at, reporting Ready so the provisioning flow passes its cluster gate.
func readyMariaDBCluster() *mariadbv1alpha1.MariaDB {
	return &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: testMariaDBName, Namespace: testNamespace},
		Status:     mariadbv1alpha1.MariaDBStatus{Conditions: []metav1.Condition{readyCondition()}},
	}
}

// The object names the two blocks provision. The nova_api block is named after
// apiInstanceName, the cell block after the CR, and cell0 after
// database.AdditionalResourceName.
const (
	apiBlockName  = testNovaName + "-api"
	cellBlockName = testNovaName
	cell0Name     = testNovaName + "-nova-cell0"
)

// markMariaDBChildrenReady flips every MariaDB Database, User and Grant in the
// fixture namespace to Ready. The provisioning flow gates each step on that
// condition, so a test driving more than one step has to stand in for the
// mariadb-operator between passes. The status is written with a plain Update:
// the fake client carries no status subresource for the MariaDB kinds, so the
// whole object is stored as given.
func markMariaDBChildrenReady(t *testing.T, c client.Client) {
	t.Helper()
	g := NewGomegaWithT(t)
	ctx := context.Background()
	inNamespace := client.InNamespace(testNamespace)

	var databases mariadbv1alpha1.DatabaseList
	g.Expect(c.List(ctx, &databases, inNamespace)).To(Succeed())
	for i := range databases.Items {
		databases.Items[i].Status.Conditions = []metav1.Condition{readyCondition()}
		g.Expect(c.Update(ctx, &databases.Items[i])).To(Succeed())
	}

	var users mariadbv1alpha1.UserList
	g.Expect(c.List(ctx, &users, inNamespace)).To(Succeed())
	for i := range users.Items {
		users.Items[i].Status.Conditions = []metav1.Condition{readyCondition()}
		g.Expect(c.Update(ctx, &users.Items[i])).To(Succeed())
	}

	var grants mariadbv1alpha1.GrantList
	g.Expect(c.List(ctx, &grants, inNamespace)).To(Succeed())
	for i := range grants.Items {
		grants.Items[i].Status.Conditions = []metav1.Condition{readyCondition()}
		g.Expect(c.Update(ctx, &grants.Items[i])).To(Succeed())
	}
}

// settleDatabase drives reconcileDatabase until both blocks are provisioned,
// marking the MariaDB CRs each pass applied Ready in between, and returns the
// first result past the provisioning gate. Provisioning walks one object per
// pass by design (a Grant is never applied before its schema and its user
// exist), so a test that wants the migration Job has to run the walk first.
func settleDatabase(t *testing.T, r *NovaReconciler, nova *novav1alpha1.Nova,
	configMapName string,
) (ctrl.Result, error) {
	t.Helper()
	g := NewGomegaWithT(t)

	// Ten passes cover the eight objects of the two blocks plus the pass that
	// finds them all ready; the bound is what keeps a regression from looping.
	for pass := 0; pass < 12; pass++ {
		res, err := r.reconcileDatabase(context.Background(), r.Client, nova, configMapName)
		cond := novaCondition(nova, "DatabaseReady")
		if err != nil || cond == nil || cond.Reason != database.ReasonWaitingForDatabase {
			return res, err
		}
		markMariaDBChildrenReady(t, r.Client)
	}
	g.Expect(novaCondition(nova, "DatabaseReady").Reason).NotTo(Equal(database.ReasonWaitingForDatabase),
		"provisioning did not settle: the flow is still waiting for a MariaDB CR after 12 passes")
	return ctrl.Result{}, nil
}

// upgradingNova returns a Nova mid-upgrade: the installed release is 2025.2, the
// spec requests 2026.1 (both the OpenStack release and the image tag, per the
// operator's bump-in-lockstep contract), and the given phase is active.
func upgradingNova(phase commonv1.UpgradePhase) *novav1alpha1.Nova {
	nova := validNova()
	nova.Spec.OpenStackRelease = "2026.1"
	nova.Spec.Image.Tag = "2026.1"
	nova.Status.InstalledRelease = "2025.2"
	nova.Status.TargetRelease = "2026.1"
	nova.Status.UpgradePhase = phase
	return nova
}

// completedJob marks a copy of desired complete, with the pod-spec hash the
// runner compares against and a stable UID for the terminal-state dedupe.
func completedJob(desired *batchv1.Job, uid string) *batchv1.Job {
	now := metav1.Now()
	j := desired.DeepCopy()
	j.UID = types.UID(uid)
	j.Annotations = map[string]string{job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template)}
	j.Status.Succeeded = 1
	j.Status.CompletionTime = &now
	j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	return j
}

// TestNovaMaxUserConnections pins the connection-cap arithmetic. The cap is what
// the operator asks mariadb-operator for, and a process that finds it exhausted
// does not degrade: it fails its pool with MySQL error 1226 and crash-loops, so
// every term is spelled out here.
func TestNovaMaxUserConnections(t *testing.T) {
	cases := []struct {
		name    string
		nova    func() *novav1alpha1.Nova
		block   dbBlock
		want    int32
		because string
	}{
		{
			name: "the default topology, nova_api", nova: validNova, block: blockAPI, want: 44,
			because: "(3+1)×2×1 API plus (1+1)×2×1 metadata plus (1+1)×2 scheduler plus (1+1)×2 conductor " +
				"is 20 sessions, doubled for the pool behind each, plus 4 for the two schemas' migration Jobs",
		},
		{
			name: "the default topology, cell", nova: validNova, block: blockCell, want: 48,
			because: "the console proxy reads its console tokens out of the cell schema alone: " +
				"one pod plus its rolling-update surge, doubled",
		},
		{
			name: "an HPA owns the API replica count, nova_api",
			nova: func() *novav1alpha1.Nova {
				nova := validNova()
				nova.Spec.Autoscaling = &novav1alpha1.AutoscalingSpec{MaxReplicas: 10}
				return nova
			},
			block: blockAPI, want: 72,
			because: "the cap has to cover the ceiling the HPA may scale to, not today's replica count",
		},
		{
			name: "an HPA owns the API replica count, cell",
			nova: func() *novav1alpha1.Nova {
				nova := validNova()
				nova.Spec.Autoscaling = &novav1alpha1.AutoscalingSpec{MaxReplicas: 10}
				return nova
			},
			block: blockCell, want: 76,
			because: "the console proxy's four are added on top of the raised API ceiling",
		},
		{
			name: "a disabled console proxy, cell",
			nova: func() *novav1alpha1.Nova {
				nova := validNova()
				nova.Spec.ConsoleProxy = novav1alpha1.NovaConsoleProxySpec{Enabled: ptr.To(false)}
				return nova
			},
			block: blockCell, want: 44,
			because: "a proxy that is not projected opens no connection to reserve",
		},
		{
			name: "a raised console proxy, cell",
			nova: func() *novav1alpha1.Nova {
				nova := validNova()
				nova.Spec.ConsoleProxy.Deployment = &novav1alpha1.DeploymentSpec{Replicas: 3}
				return nova
			},
			block: blockCell, want: 52,
			because: "console proxies are peers, and each one holds its own pair",
		},
		{
			name: "a wider uWSGI topology, nova_api",
			nova: func() *novav1alpha1.Nova {
				nova := validNova()
				nova.Spec.API.UWSGI = &novav1alpha1.UWSGISpec{Processes: 4, Threads: 2}
				return nova
			},
			block: blockAPI, want: 92,
			because: "the API floor scales with processes × threads, which is where the cap is spent",
		},
		{
			name: "a CR that bypassed the defaulting webhook, cell", nova: novaMinimal, block: blockCell, want: 72,
			because: "an unset replica count runs the shared default of three pods, which is what the workload " +
				"builder projects, so all four fleets are sized for three, and an absent console-proxy block is " +
				"the one replica the webhook would have materialized",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(novaMaxUserConnections(tc.nova(), tc.block)).To(Equal(tc.want), tc.because)
		})
	}
}

// TestReconcileDatabase_ProvisionsBothSchemasInOrder covers the full
// provisioning walk. Nova is the first operator to provision two blocks and an
// additional schema, so what this pins is that the cell block carries cell0 as a
// Database and a Grant of its own rather than as a block with its own user: the
// conductor reads the instances out of the cell schema and the holding pen out
// of cell0 with the same credentials.
func TestReconcileDatabase_ProvisionsBothSchemasInOrder(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	r := newNovaTestReconciler(nova, readyMariaDBCluster())

	_, err := settleDatabase(t, r, nova, testConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())

	ctx := context.Background()
	for _, name := range []string{apiBlockName, cellBlockName} {
		var db mariadbv1alpha1.Database
		g.Expect(r.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: name}, &db)).To(Succeed())
		var user mariadbv1alpha1.User
		g.Expect(r.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: name}, &user)).To(Succeed())
		var grant mariadbv1alpha1.Grant
		g.Expect(r.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: name}, &grant)).To(Succeed())
	}

	var apiDB mariadbv1alpha1.Database
	g.Expect(r.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: apiBlockName}, &apiDB)).To(Succeed())
	g.Expect(apiDB.Spec.Name).To(Equal("nova_api"))

	var apiUser mariadbv1alpha1.User
	g.Expect(r.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: apiBlockName}, &apiUser)).To(Succeed())
	g.Expect(apiUser.Spec.MaxUserConnections).To(Equal(novaMaxUserConnections(nova, blockAPI)))

	var cellUser mariadbv1alpha1.User
	g.Expect(r.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: cellBlockName}, &cellUser)).To(Succeed())
	g.Expect(cellUser.Spec.MaxUserConnections).To(Equal(novaMaxUserConnections(nova, blockCell)))

	// cell0: one more Database and one more Grant on the cell block's user, and
	// deliberately no User of its own.
	var cell0DB mariadbv1alpha1.Database
	g.Expect(r.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: cell0Name}, &cell0DB)).To(Succeed())
	g.Expect(cell0DB.Spec.Name).To(Equal("nova_cell0"))

	var cell0Grant mariadbv1alpha1.Grant
	g.Expect(r.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: cell0Name}, &cell0Grant)).To(Succeed())
	g.Expect(cell0Grant.Spec.Database).To(Equal("nova_cell0"))
	g.Expect(cell0Grant.Spec.Username).To(Equal(testNovaName),
		"cell0 is granted to the cell block's user: the conductor reads both schemas as one identity")

	var cell0User mariadbv1alpha1.User
	err = r.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: cell0Name}, &cell0User)
	g.Expect(err).To(HaveOccurred(), "an additional schema gets no user of its own")
}

// TestReconcileDatabase_Cell0FollowsTheCellSchema covers a cell schema that is
// not called nova. cell0's name is derived from it in two places that have to
// agree: the schema the provisioning flow creates and the schema map_cell0
// writes into the cell map. A fixed nova_cell0 in either maps cell0 onto a
// schema that was never created, and the db sync that migrates cell0 behind it
// fails on every pass.
func TestReconcileDatabase_Cell0FollowsTheCellSchema(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	nova.Spec.Database.Database = "nova_eu"
	r := newNovaTestReconciler(nova, readyMariaDBCluster())

	g.Expect(dbSyncScript(nova)).To(ContainSubstring(
		"cell_v2 map_cell0 --database_connection " +
			"'{scheme}://{username}:{password}@{hostname}:{port}/nova_eu_cell0?{query}'"))

	_, err := settleDatabase(t, r, nova, testConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())

	var cell0DB mariadbv1alpha1.Database
	key := client.ObjectKey{Namespace: testNamespace, Name: testNovaName + "-nova-eu-cell0"}
	g.Expect(r.Get(context.Background(), key, &cell0DB)).To(Succeed())
	g.Expect(cell0DB.Spec.Name).To(Equal("nova_eu_cell0"))
}

// TestReconcileDatabase_APIBlockNotReadyStopsBeforeCell covers the ordering
// between the two blocks: the nova_api schema holds the cell map, so a cell
// schema provisioned ahead of it would be a database nothing can address.
func TestReconcileDatabase_APIBlockNotReadyStopsBeforeCell(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	r := newNovaTestReconciler(nova, readyMariaDBCluster())

	res, err := r.reconcileDatabase(context.Background(), r.Client, nova, testConfigMapName)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	cond := novaCondition(nova, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonWaitingForDatabase))

	var cellDB mariadbv1alpha1.Database
	err = r.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: testNovaName}, &cellDB)
	g.Expect(err).To(HaveOccurred(), "the cell schema must wait for the nova_api block to report ready")
}

// TestReconcileDatabase_NoConfigWaits covers the pass before the config step has
// rendered anything: the migration Jobs mount that ConfigMap as their whole
// config directory, so an empty name must wait instead of creating a Job the API
// server refuses.
func TestReconcileDatabase_NoConfigWaits(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	r := newNovaTestReconciler(nova, readyMariaDBCluster())

	res, err := settleDatabase(t, r, nova, "")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	cond := novaCondition(nova, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonDatabaseWaitingForConfig))

	var jobs batchv1.JobList
	g.Expect(r.List(context.Background(), &jobs)).To(Succeed())
	g.Expect(jobs.Items).To(BeEmpty(), "no Job may be built against an empty config mount")
}

// TestCheckImageReleaseMismatch covers the decoupled-field contract between
// spec.openStackRelease and spec.image: they drive different things, and a
// disagreement would promote an installed-release marker for a release the pods
// do not run.
func TestCheckImageReleaseMismatch(t *testing.T) {
	cases := []struct {
		name    string
		tag     string
		release string
		blocked bool
		because string
	}{
		{name: "in lockstep", tag: "2026.1", release: "2026.1", blocked: false},
		{
			name: "a patched image build", tag: "2026.1-p1", release: "2026.1", blocked: false,
			because: "the patch suffix names a rebuild of the same release",
		},
		{
			name: "a digest-pinned image", tag: "", release: "2026.1", blocked: false,
			because: "a digest carries no release string, so the explicit declaration stands",
		},
		{
			name: "an unparseable tag", tag: "latest", release: "2026.1", blocked: false,
			because: "nothing comparable, so release tracking is left to spec.openStackRelease",
		},
		{
			name: "a lagging image", tag: "2025.2", release: "2026.1", blocked: true,
			because: "the migration Jobs would run the old nova-manage against the new schema",
		},
		{
			name: "an image a minor ahead in the same year", tag: "2026.2", release: "2026.1", blocked: true,
			because: "a matching year still names two releases, so the minor has to match as well",
		},
		{
			name: "an image on the same minor a year behind", tag: "2025.1", release: "2026.1", blocked: true,
			because: "every year has a .1, so the year has to match as well",
		},
		{
			name: "an unparseable release", tag: "2026.1", release: "bogus", blocked: false,
			because: "there is no release to compare the tag against, and admission is what rejects the release itself",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			nova := validNova()
			nova.Spec.Image.Tag = tc.tag
			nova.Spec.OpenStackRelease = tc.release

			res, blocked := checkImageReleaseMismatch(nova)

			g.Expect(blocked).To(Equal(tc.blocked), tc.because)
			if !tc.blocked {
				g.Expect(res.IsZero()).To(BeTrue())
				g.Expect(novaCondition(nova, "DatabaseReady")).To(BeNil())
				return
			}
			g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
			cond := novaCondition(nova, "DatabaseReady")
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(conditionReasonImageReleaseMismatch))
			g.Expect(cond.Message).To(ContainSubstring("bump spec.image in lockstep"))
		})
	}
}

// TestReconcileDatabase_ImageReleaseMismatchBlocksTheSync covers the contract
// on the steady-state path, where no upgrade is in flight to catch a
// disagreement: the db-sync Job would migrate with the image's nova-manage and
// then promote the installed-release marker to a release that image is not,
// which is where the next upgrade would start from.
func TestReconcileDatabase_ImageReleaseMismatchBlocksTheSync(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova() // spec.openStackRelease 2025.2, nothing installed yet
	nova.Spec.Image.Tag = "2025.1"
	r := newNovaTestReconciler(nova, readyMariaDBCluster())

	res, err := settleDatabase(t, r, nova, testConfigMapName)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
	cond := novaCondition(nova, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonImageReleaseMismatch))
	g.Expect(nova.Status.InstalledRelease).To(BeEmpty())

	var jobs batchv1.JobList
	g.Expect(r.List(context.Background(), &jobs)).To(Succeed())
	g.Expect(jobs.Items).To(BeEmpty(), "no migration may run from an image the release does not name")
}

// TestReconcileDatabase_DowngradeReportsDowngradeNotSupported covers the spec
// release edited backwards. The expand phase widens the schema and the contract
// phase drops what the old release still reads, so there is no phase sequence
// that walks back: the shared flow refuses the transition rather than running
// the migrations in reverse.
func TestReconcileDatabase_DowngradeReportsDowngradeNotSupported(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova() // spec.openStackRelease 2025.2
	nova.Status.InstalledRelease = "2026.1"
	r := newNovaTestReconciler(nova, readyMariaDBCluster())

	_, err := settleDatabase(t, r, nova, testConfigMapName)

	g.Expect(err).To(MatchError(ContainSubstring("downgrade from 2026.1 to 2025.2 is not supported")))
	cond := novaCondition(nova, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonDowngradeNotSupported))
	g.Expect(nova.Status.UpgradePhase).To(BeEmpty(), "a refused transition starts no phase walk")
}

// TestReconcileDatabase_SyncJobCommandAndEnv pins the steady-state Job: the
// db-sync script, the config mount every migration reads its database
// coordinates from, and the two connection overrides that keep the credentials
// out of the rendered file.
func TestReconcileDatabase_SyncJobCommandAndEnv(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	r := newNovaTestReconciler(nova, readyMariaDBCluster())

	_, err := settleDatabase(t, r, nova, testConfigMapName)
	g.Expect(err).NotTo(HaveOccurred())

	var syncJob batchv1.Job
	key := client.ObjectKey{Namespace: testNamespace, Name: testNovaName + "-" + dbSyncJobSuffix}
	g.Expect(r.Get(context.Background(), key, &syncJob)).To(Succeed())

	container := syncJob.Spec.Template.Spec.Containers[0]
	g.Expect(container.Command).To(Equal([]string{"/bin/sh", "-eu", "-c", dbSyncScript(nova)}))
	g.Expect(container.Image).To(Equal(nova.Spec.Image.Reference()))

	var mountPaths []string
	for _, mount := range container.VolumeMounts {
		mountPaths = append(mountPaths, mount.MountPath)
	}
	g.Expect(mountPaths).To(ContainElement(novaConfigDir))

	g.Expect(envNames(container.Env)).To(ContainElements(
		"OS_API_DATABASE__CONNECTION",
		database.ConnectionEnvVarName,
		"OS_DEFAULT__TRANSPORT_URL",
		"OS_KEYSTONE_AUTHTOKEN__PASSWORD",
	))
}

// envNames returns the names of the given environment, which is what the
// per-role assertions compare against.
func envNames(env []corev1.EnvVar) []string {
	names := make([]string, 0, len(env))
	for _, e := range env {
		names = append(names, e.Name)
	}
	return names
}

// envValue returns the literal value of the named variable, or "" when it is
// absent or sourced from a Secret.
func envValue(env []corev1.EnvVar, name string) string {
	for _, e := range env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

// TestNovaWorkloadEnv_PerRole covers what separates the six roles. Every process
// reads the same bus and the same cell schema, so the differences are the point:
// the console proxy never opens nova_api, only the metadata API verifies the
// proxy signature, and only the two HTTP front ends load a paste pipeline.
func TestNovaWorkloadEnv_PerRole(t *testing.T) {
	const (
		apiDBConnection  = "OS_API_DATABASE__CONNECTION"
		sharedSecret     = "OS_NEUTRON__METADATA_PROXY_SHARED_SECRET"
		configDir        = "OS_NOVA_CONFIG_DIR"
		configFiles      = "OS_NOVA_CONFIG_FILES"
		apiConfigFiles   = "/var/lib/openstack/etc/nova/api-paste.ini;nova.conf"
		metaConfigFiles  = "/var/lib/openstack/etc/nova/api-paste.ini;nova.conf;metadata.conf"
		cinderPassword   = "OS_CINDER__PASSWORD"
		placementPasswrd = "OS_PLACEMENT__PASSWORD"
	)
	always := []string{
		"OS_DEFAULT__TRANSPORT_URL",
		database.ConnectionEnvVarName,
		"OS_KEYSTONE_AUTHTOKEN__PASSWORD",
		"OS_SERVICE_USER__PASSWORD",
		placementPasswrd,
		"OS_NEUTRON__PASSWORD",
	}

	cases := []struct {
		role        workloadRole
		present     []string
		absent      []string
		configFiles string
	}{
		{
			role:        roleAPI,
			present:     []string{apiDBConnection, configDir, configFiles},
			absent:      []string{sharedSecret},
			configFiles: apiConfigFiles,
		},
		{
			role:        roleMetadata,
			present:     []string{apiDBConnection, sharedSecret, configDir, configFiles},
			configFiles: metaConfigFiles,
		},
		{role: roleScheduler, present: []string{apiDBConnection}, absent: []string{sharedSecret, configDir, configFiles}},
		{role: roleConductor, present: []string{apiDBConnection}, absent: []string{sharedSecret, configDir, configFiles}},
		{role: roleManage, present: []string{apiDBConnection}, absent: []string{sharedSecret, configDir, configFiles}},
		{role: roleConsoleProxy, absent: []string{apiDBConnection, sharedSecret, configDir, configFiles}},
	}
	for _, tc := range cases {
		t.Run(string(tc.role), func(t *testing.T) {
			g := NewGomegaWithT(t)
			nova := validNova()

			env := novaWorkloadEnv(nova, tc.role)

			names := envNames(env)
			g.Expect(names).To(ContainElements(always))
			if len(tc.present) > 0 {
				g.Expect(names).To(ContainElements(tc.present))
			}
			for _, absent := range tc.absent {
				g.Expect(names).NotTo(ContainElement(absent))
			}
			if tc.configFiles != "" {
				g.Expect(envValue(env, configDir)).To(Equal(novaConfigDir))
				g.Expect(envValue(env, configFiles)).To(Equal(tc.configFiles),
					"the paste configuration is the first entry deploy.loadapp reads")
			}
		})
	}

	// The console proxy reads console tokens out of the cell schema, so its one
	// database override is the cell DSN.
	t.Run("the console proxy keeps the cell DSN", func(t *testing.T) {
		g := NewGomegaWithT(t)
		env := novaWorkloadEnv(validNova(), roleConsoleProxy)
		g.Expect(envNames(env)).To(ContainElement(database.ConnectionEnvVarName))
	})

	// The two optional client integrations are the only per-CR difference in the
	// password set: a deployment without block storage has no [cinder] section to
	// override, and Glance and Barbican carry no password at all.
	t.Run("cinder's password is rendered only when the integration is on", func(t *testing.T) {
		g := NewGomegaWithT(t)
		g.Expect(envNames(novaWorkloadEnv(validNova(), roleAPI))).To(ContainElement(cinderPassword))

		minimal := envNames(novaWorkloadEnv(novaMinimal(), roleAPI))
		g.Expect(minimal).NotTo(ContainElement(cinderPassword))
		g.Expect(minimal).NotTo(ContainElement("OS_GLANCE__PASSWORD"))
		g.Expect(minimal).NotTo(ContainElement("OS_BARBICAN__PASSWORD"))
		g.Expect(minimal).To(ContainElement(placementPasswrd))
	})
}

// TestNovaJobSetParams_DatabaseTLS covers the encrypted database connections:
// each schema's DSN names ssl_ca/ssl_cert/ssl_key paths under its own mount
// point, so without both projected keypairs nova-manage cannot open them and
// every migration fails.
func TestNovaJobSetParams_DatabaseTLS(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	nova.Spec.APIDatabase.TLS = &commonv1.DatabaseTLSSpec{
		Mode:                "verify-full",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "api-db-ca"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "api-db-client"},
	}
	nova.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
		Mode:                "verify-full",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "cell-db-ca"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "cell-db-client"},
	}

	params := novaJobSetParams(nova, testConfigMapName)

	g.Expect(params.ExtraVolumes).To(HaveLen(2))
	g.Expect(params.ExtraVolumes[0].Name).To(Equal(apiDBTLSVolumeName))
	g.Expect(params.ExtraVolumes[1].Name).To(Equal(cellDBTLSVolumeName))
	g.Expect(params.ExtraVolumeMounts).To(HaveLen(2))
	g.Expect(params.ExtraVolumeMounts[0].MountPath).To(Equal(apiDBTLSMountPath))
	g.Expect(params.ExtraVolumeMounts[1].MountPath).To(Equal(cellDBTLSMountPath))

	// Each volume projects its own block's Secrets: a copy-paste would leave one
	// schema verifying against the other's authority.
	g.Expect(projectedSecretNames(params.ExtraVolumes[0])).To(Equal([]string{"api-db-ca", "api-db-client"}))
	g.Expect(projectedSecretNames(params.ExtraVolumes[1])).To(Equal([]string{"cell-db-ca", "cell-db-client"}))
	for _, volume := range params.ExtraVolumes {
		g.Expect(volume.Projected.DefaultMode).To(Equal(ptr.To(int32(0o400))),
			"%s carries a private key, which only the openstack UID may read", volume.Name)
	}

	// A disabled block projects nothing, so a Nova that keeps the certificate
	// references while turning verification off does not mount them.
	nova.Spec.APIDatabase.TLS.Mode = "disabled"
	nova.Spec.Database.TLS.Mode = "disabled"
	g.Expect(novaJobSetParams(nova, testConfigMapName).ExtraVolumes).To(BeEmpty())
}

// projectedSecretNames returns the Secrets a projected volume draws from, in
// projection order.
func projectedSecretNames(volume corev1.Volume) []string {
	names := make([]string, 0, len(volume.Projected.Sources))
	for _, source := range volume.Projected.Sources {
		names = append(names, source.Secret.Name)
	}
	return names
}

// TestNovaJobSetParams_DatabaseTLSPerBlock covers the two schemas verifying
// independently. Each block names its own MariaDB cluster, so one can run TLS
// while the other does not, and the single volume that is projected has to be
// the enabled block's own: its name, its mount point and its Secrets. Anything
// else leaves that schema's DSN pointing at ssl_ca/ssl_cert/ssl_key files that
// are missing or issued for the other cluster. The disabled block keeps its
// certificate references, so a wrong pick shows up as the wrong Secrets.
func TestNovaJobSetParams_DatabaseTLSPerBlock(t *testing.T) {
	cases := []struct {
		name      string
		apiMode   string
		cellMode  string
		volume    string
		mountPath string
		secrets   []string
	}{
		{
			name: "only nova_api verifies", apiMode: "verify-full", cellMode: "disabled",
			volume: apiDBTLSVolumeName, mountPath: apiDBTLSMountPath,
			secrets: []string{"api-db-ca", "api-db-client"},
		},
		{
			name: "only the cell schema verifies", apiMode: "disabled", cellMode: "verify-full",
			volume: cellDBTLSVolumeName, mountPath: cellDBTLSMountPath,
			secrets: []string{"cell-db-ca", "cell-db-client"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			nova := validNova()
			nova.Spec.APIDatabase.TLS = &commonv1.DatabaseTLSSpec{
				Mode:                tc.apiMode,
				CABundleSecretRef:   commonv1.SecretRefSpec{Name: "api-db-ca"},
				ClientCertSecretRef: commonv1.SecretRefSpec{Name: "api-db-client"},
			}
			nova.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
				Mode:                tc.cellMode,
				CABundleSecretRef:   commonv1.SecretRefSpec{Name: "cell-db-ca"},
				ClientCertSecretRef: commonv1.SecretRefSpec{Name: "cell-db-client"},
			}

			params := novaJobSetParams(nova, testConfigMapName)

			g.Expect(params.ExtraVolumes).To(HaveLen(1))
			g.Expect(params.ExtraVolumes[0].Name).To(Equal(tc.volume))
			g.Expect(projectedSecretNames(params.ExtraVolumes[0])).To(Equal(tc.secrets))
			g.Expect(params.ExtraVolumeMounts).To(HaveLen(1))
			g.Expect(params.ExtraVolumeMounts[0].Name).To(Equal(tc.volume))
			g.Expect(params.ExtraVolumeMounts[0].MountPath).To(Equal(tc.mountPath))
		})
	}
}

// TestBuildPhaseJob covers the four phases of the upgrade walk. Nova splits the
// work differently from the phase names: expand runs the readiness check and
// both schemas' migrations, migrate is the read that proves the new code can
// address the migrated nova_api schema, and contract runs the online data
// migrations the rolling update before it made safe.
func TestBuildPhaseJob(t *testing.T) {
	cases := []struct {
		phase   commonv1.UpgradePhase
		name    string
		command []string
	}{
		{
			phase:   commonv1.UpgradePhaseExpanding,
			name:    "nova-db-expand",
			command: []string{"/bin/sh", "-eu", "-c", upgradeExpandScript},
		},
		{
			phase:   commonv1.UpgradePhaseMigrating,
			name:    "nova-db-migrate",
			command: []string{"nova-manage", "--config-dir", "/etc/nova/nova.conf.d", "cell_v2", "list_cells"},
		},
		{
			phase:   commonv1.UpgradePhaseContracting,
			name:    "nova-db-contract",
			command: []string{"/bin/sh", "-eu", "-c", upgradeContractScript},
		},
	}
	nova := upgradingNova(commonv1.UpgradePhaseExpanding)
	r := newNovaTestReconciler(nova)
	build := r.upgradeFlowParams(context.Background(), r.Client, nova, testConfigMapName).BuildPhaseJob

	for _, tc := range cases {
		t.Run(string(tc.phase), func(t *testing.T) {
			g := NewGomegaWithT(t)
			phaseJob := build(tc.phase)

			g.Expect(phaseJob).NotTo(BeNil())
			g.Expect(phaseJob.Name).To(Equal(tc.name))
			g.Expect(*phaseJob.Spec.BackoffLimit).To(Equal(upgradePhaseJobBackoffLimit))

			container := phaseJob.Spec.Template.Spec.Containers[0]
			g.Expect(container.Command).To(Equal(tc.command))
			g.Expect(container.Image).To(Equal(nova.Spec.Image.Reference()),
				"every phase runs the target release's image")
			g.Expect(container.Env).To(Equal(novaWorkloadEnv(nova, roleManage)))

			var mountPaths []string
			for _, mount := range container.VolumeMounts {
				mountPaths = append(mountPaths, mount.MountPath)
			}
			g.Expect(mountPaths).To(ContainElement(novaConfigDir))
		})
	}

	t.Run("no Job outside the three migration phases", func(t *testing.T) {
		g := NewGomegaWithT(t)
		g.Expect(build(commonv1.UpgradePhaseRollingUpdate)).To(BeNil(),
			"the Deployment rollout drives the rolling update, not a Job")
		g.Expect(build("")).To(BeNil())
	})
}

// TestReconcileDatabase_InstalledReleasePromotedOnSuccess covers the
// steady-state completion: the marker moves to spec.openStackRelease and the
// terminal metric stamps its per-phase dedupe annotation.
func TestReconcileDatabase_InstalledReleasePromotedOnSuccess(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova() // InstalledRelease empty (fresh), OpenStackRelease 2025.2
	completed := completedJob(database.SyncJob(novaJobSetParams(nova, testConfigMapName)), "sync-job-uid")
	r := newNovaTestReconciler(nova, readyMariaDBCluster(), completed)

	res, err := settleDatabase(t, r, nova, testConfigMapName)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(nova.Status.InstalledRelease).To(Equal("2025.2"))
	cond := novaCondition(nova, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(database.ReasonDatabaseSynced))
	g.Expect(nova.Annotations).To(HaveKey(dbJobUIDAnnotationKey(dbSyncJobSuffix)))
	g.Expect(nova.Annotations).To(HaveKey(dbJobUIDAnnotationKey(cellsReportJobSuffix)),
		"the cell report runs off the same completed Job")
}

// TestReconcileDatabase_FailedSyncReportsNoCells covers a db-sync that used up
// its retries. The failure has to surface as DBSyncFailed and as an error the
// caller retries on, and the cell report has to stay out of it: a failed Job
// mapped nothing, so reading its report would emit a spurious
// CellsReportUnavailable and spend the report's once-per-Job stamp on a Job
// that never had cells to report.
func TestReconcileDatabase_FailedSyncReportsNoCells(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	desired := database.SyncJob(novaJobSetParams(nova, testConfigMapName))
	failed := desired.DeepCopy()
	failed.UID = "failed-sync-job-uid"
	failed.Annotations = map[string]string{job.PodSpecHashAnnotation: job.PodSpecHash(&desired.Spec.Template)}
	failed.Status.Failed = 1
	failed.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	r := newNovaTestReconciler(nova, readyMariaDBCluster(), failed)

	_, err := settleDatabase(t, r, nova, testConfigMapName)

	g.Expect(err).To(MatchError(job.ErrJobFailed))
	g.Expect(err).To(MatchError(ContainSubstring("running db_sync")))
	cond := novaCondition(nova, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonDBSyncFailed))
	g.Expect(nova.Status.InstalledRelease).To(BeEmpty())
	g.Expect(nova.Annotations).To(HaveKey(dbJobUIDAnnotationKey(dbSyncJobSuffix)),
		"a failed Job is counted by the terminal metric like a completed one")
	g.Expect(nova.Annotations).NotTo(HaveKey(dbJobUIDAnnotationKey(cellsReportJobSuffix)),
		"only a completed db-sync carries a cell report")
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).NotTo(
		ContainElement(ContainSubstring("CellsReportUnavailable")))
}

// TestReconcileDatabase_UpgradeWalk covers the release bump: the initiation off
// the steady-state path, the expand phase completing into the migrate phase, and
// the mid-upgrade image drift that freezes it.
func TestReconcileDatabase_UpgradeWalk(t *testing.T) {
	t.Run("a release bump initiates the upgrade", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := validNova() // spec.openStackRelease 2025.2
		nova.Status.InstalledRelease = "2025.1"
		r := newNovaTestReconciler(nova, readyMariaDBCluster())

		_, err := settleDatabase(t, r, nova, testConfigMapName)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(nova.Status.UpgradePhase).To(Equal(commonv1.UpgradePhaseExpanding))
		g.Expect(nova.Status.TargetRelease).To(Equal("2025.2"))
	})

	t.Run("the expand phase completes into the migrate phase", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := upgradingNova(commonv1.UpgradePhaseExpanding)
		params := novaJobSetParams(nova, testConfigMapName)
		desired := database.BuildJob(params, nova.Spec.Image.Reference(), upgradeExpandJobSuffix,
			[]string{"/bin/sh", "-eu", "-c", upgradeExpandScript}, upgradePhaseJobBackoffLimit)
		r := newNovaTestReconciler(nova, readyMariaDBCluster(), completedJob(desired, "expand-job-uid"))

		res, err := settleDatabase(t, r, nova, testConfigMapName)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}))
		g.Expect(nova.Status.UpgradePhase).To(Equal(commonv1.UpgradePhaseMigrating))
		// Both terminal reporters ran: the shared metric and the check report.
		g.Expect(nova.Annotations).To(HaveKey(dbJobUIDAnnotationKey(upgradeExpandJobSuffix)))
		g.Expect(nova.Annotations).To(HaveKey(dbJobUIDAnnotationKey(upgradeCheckJobSuffix)))
	})

	// Mid-upgrade the image and the release must stay in lockstep too: a lone
	// image edit would dispatch phase Jobs built from the wrong binary.
	t.Run("a mid-upgrade image drift blocks", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := upgradingNova(commonv1.UpgradePhaseExpanding)
		nova.Spec.Image.Tag = "2025.2"
		r := newNovaTestReconciler(nova, readyMariaDBCluster())

		res, err := settleDatabase(t, r, nova, testConfigMapName)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(RequeueDatabaseWait))
		g.Expect(novaCondition(nova, "DatabaseReady").Reason).To(Equal(conditionReasonImageReleaseMismatch))
		g.Expect(nova.Status.UpgradePhase).To(Equal(commonv1.UpgradePhaseExpanding),
			"the phase is frozen until the image is bumped in lockstep")
	})
}

// TestReconcileDatabase_AbortReachableDuringImageDrift covers the way out of a
// wedged upgrade. The abort is spec.openStackRelease reverted to the installed
// release, and the image tag is typically still on the target being backed out
// of: a mismatch check that ran ahead of the abort would hold the upgrade on
// ImageReleaseMismatch, so the one edit meant to unstick it could never land.
func TestReconcileDatabase_AbortReachableDuringImageDrift(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := upgradingNova(commonv1.UpgradePhaseExpanding)
	nova.Spec.OpenStackRelease = nova.Status.InstalledRelease // spec.image.tag stays on 2026.1
	r := newNovaTestReconciler(nova, readyMariaDBCluster())

	// The abort writes no condition of its own, so settling against the real
	// config name would run a steady-state pass right behind it. Settling the
	// provisioning without a config first leaves the abort as the only pass.
	_, err := settleDatabase(t, r, nova, "")
	g.Expect(err).NotTo(HaveOccurred())

	res, err := r.reconcileDatabase(context.Background(), r.Client, nova, testConfigMapName)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}))
	g.Expect(nova.Status.UpgradePhase).To(BeEmpty())
	g.Expect(nova.Status.TargetRelease).To(BeEmpty())
	g.Expect(nova.Status.InstalledRelease).To(Equal("2025.2"))
	g.Expect(novaCondition(nova, "DatabaseReady").Reason).NotTo(Equal(conditionReasonImageReleaseMismatch),
		"a revert to the installed release must not be blocked by the image mismatch check")
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(
		ContainElement(ContainSubstring(corev1.EventTypeNormal + " " + database.ReasonUpgradeAborted)))
}

// --- the cell report and the nova-status upgrade check ---

// terminatedJobPod returns a Job's pod with the given termination message on its
// only container.
func terminatedJobPod(jobName, message string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName + "-pod",
			Namespace: testNamespace,
			Labels:    map[string]string{"batch.kubernetes.io/job-name": jobName},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "migration",
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{Message: message},
				},
			}},
		},
	}
}

// terminalJob returns a completed Job with the given name suffix and UID, which
// is what the per-Job dedupe keys the report on.
func terminalJob(suffix, uid string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testNovaName + "-" + suffix,
			Namespace: testNamespace,
			UID:       types.UID(uid),
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}},
		},
	}
}

// TestParseCellsReport covers what the operator makes of the db-sync Job's
// termination log. The UUIDs are what a per-cell "nova-manage cell_v2" command
// addresses a cell by, so a line that is not a mapping must never reach
// status.cells as one.
func TestParseCellsReport(t *testing.T) {
	cases := []struct {
		name    string
		message string
		want    []novav1alpha1.NovaCellStatus
		because string
	}{
		{
			name:    "both cells, in the order list_cells printed them",
			message: "cell0=" + testCell0UUID + "\ncell1=" + testCell1UUID,
			want: []novav1alpha1.NovaCellStatus{
				{Name: "cell0", UUID: testCell0UUID},
				{Name: "cell1", UUID: testCell1UUID},
			},
			because: "cell0 comes first, which is the order the status field documents",
		},
		{
			name:    "a trailing newline",
			message: "cell0=" + testCell0UUID + "\n",
			want:    []novav1alpha1.NovaCellStatus{{Name: "cell0", UUID: testCell0UUID}},
			because: "the awk stage ends every line, including the last one",
		},
		{name: "no report at all", message: ""},
		{
			name: "a line without a mapping", message: "nova-manage ran",
			because: "anything nova-manage wrote to the same file is not a cell",
		},
		{
			name: "a value that is not a UUID", message: "cell1=not-a-uuid",
			because: "a truncated report would publish an identifier no command accepts",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(parseCellsReport(tc.message)).To(Equal(tc.want), tc.because)
		})
	}
}

// TestReportCells_WritesStatusFromTheTerminationMessage covers the happy path:
// the cells the db-sync Job mapped reach status.cells with the UUIDs nova
// generated for them.
func TestReportCells_WritesStatusFromTheTerminationMessage(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	observed := terminalJob(dbSyncJobSuffix, "cells-uid")
	message := "cell0=" + testCell0UUID + "\ncell1=" + testCell1UUID
	r := newNovaTestReconciler(nova, terminatedJobPod(observed.Name, message))

	r.reportCells(context.Background(), r.Client, nova, observed)

	g.Expect(nova.Status.Cells).To(Equal([]novav1alpha1.NovaCellStatus{
		{Name: "cell0", UUID: testCell0UUID},
		{Name: "cell1", UUID: testCell1UUID},
	}))
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(BeEmpty())
}

// TestReportCells_UnavailableEmitsEventAndKeepsStatus covers the two reads that
// yield nothing. The cells are mapped either way, so dropping a previously
// published UUID would be worse than reporting a stale one.
func TestReportCells_UnavailableEmitsEventAndKeepsStatus(t *testing.T) {
	published := []novav1alpha1.NovaCellStatus{{Name: "cell0", UUID: testCell0UUID}}

	cases := []struct {
		name string
		pod  *corev1.Pod
	}{
		{name: "no pod at all"},
		{name: "a pod that wrote no message", pod: terminatedJobPod(testNovaName+"-"+dbSyncJobSuffix, "")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			nova := validNova()
			nova.Status.Cells = published
			objs := []client.Object{nova}
			if tc.pod != nil {
				objs = append(objs, tc.pod)
			}
			r := newNovaTestReconciler(objs...)

			r.reportCells(context.Background(), r.Client, nova, terminalJob(dbSyncJobSuffix, "cells-uid-"+tc.name))

			g.Expect(nova.Status.Cells).To(Equal(published), "an unreadable report leaves the last one standing")
			g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(ConsistOf(
				ContainSubstring("Normal CellsReportUnavailable")))
		})
	}
}

// TestReportCells_PicksTheReportingPodAmongRetries covers a db-sync that failed
// once before it completed. The Job runs each retry as a pod of its own, the
// failed attempt wrote no report, and the list comes back in no fixed order: a
// read of whichever pod comes first would report the cells unavailable, and the
// per-Job-UID dedupe would make that final. The failed pod's name sorts first
// here, and it is the newer one, so neither order nor age picks the report.
func TestReportCells_PicksTheReportingPodAmongRetries(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	observed := terminalJob(dbSyncJobSuffix, "cells-uid-retries")

	failed := terminatedJobPod(observed.Name, "")
	failed.Name = observed.Name + "-aaaaa"
	failed.CreationTimestamp = metav1.NewTime(time.Now())
	failed.Status.Phase = corev1.PodFailed

	succeeded := terminatedJobPod(observed.Name, "cell0="+testCell0UUID+"\ncell1="+testCell1UUID)
	succeeded.Name = observed.Name + "-zzzzz"
	succeeded.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Minute))
	succeeded.Status.Phase = corev1.PodSucceeded

	r := newNovaTestReconciler(nova, failed, succeeded)

	r.reportCells(context.Background(), r.Client, nova, observed)

	g.Expect(nova.Status.Cells).To(HaveLen(2))
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(BeEmpty())
}

// TestJobPodTerminationMessage_NewestPodWithoutASuccess covers a Job none of
// whose pods succeeded, which is how a failed expand phase ends: the newest
// attempt carries the check's last word.
func TestJobPodTerminationMessage_NewestPodWithoutASuccess(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	jobName := testNovaName + "-" + upgradeExpandJobSuffix

	older := terminatedJobPod(jobName, "nova-status upgrade check exit 2")
	older.Name = jobName + "-zzzzz"
	older.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Minute))
	older.Status.Phase = corev1.PodFailed

	newer := terminatedJobPod(jobName, "nova-status upgrade check exit 3")
	newer.Name = jobName + "-aaaaa"
	newer.CreationTimestamp = metav1.NewTime(time.Now())
	newer.Status.Phase = corev1.PodFailed

	r := newNovaTestReconciler(nova, older, newer)

	g.Expect(r.jobPodTerminationMessage(context.Background(), r.Client, nova, jobName)).
		To(Equal("nova-status upgrade check exit 3"))
}

// TestReportCells_PodListFailureIsUnavailable covers the read that fails
// outright: the message is diagnostic, so the reconcile goes on, and the report
// is unavailable exactly like a pod that wrote nothing.
func TestReportCells_PodListFailureIsUnavailable(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	c := novaFakeClientBuilder(nova).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.PodList); ok {
				return errors.New("pods are forbidden")
			}
			return cl.List(ctx, list, opts...)
		},
	}).Build()
	r := &NovaReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}

	r.reportCells(context.Background(), r.Client, nova, terminalJob(dbSyncJobSuffix, "cells-uid-list-error"))

	g.Expect(nova.Status.Cells).To(BeNil())
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(ConsistOf(
		ContainSubstring("Normal CellsReportUnavailable")))
}

// TestReportCells_OncePerJobUID covers the dedupe: the steady-state path
// re-observes the same completed Job on every pass, and a report that ran each
// time would rewrite the status and re-emit the event forever.
func TestReportCells_OncePerJobUID(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	observed := terminalJob(dbSyncJobSuffix, "cells-uid-dedupe")
	r := newNovaTestReconciler(nova, terminatedJobPod(observed.Name, "cell0="+testCell0UUID))

	r.reportCells(context.Background(), r.Client, nova, observed)
	nova.Status.Cells = nil
	r.reportCells(context.Background(), r.Client, nova, observed)

	g.Expect(nova.Status.Cells).To(BeNil(), "the second pass must not write the status again")
}

// TestRecordUpgradePhaseTerminal covers what the operator makes of the exit code
// the expand Job deliberately swallows: a clean run is Normal, warnings are a
// Warning carrying the message, and a pod whose message cannot be read says so
// rather than leaving a silent gap.
func TestRecordUpgradePhaseTerminal(t *testing.T) {
	cases := []struct {
		name      string
		message   string
		eventType string
		reason    string
		expect    string
	}{
		{
			name: "a clean check", message: "nova-status upgrade check exit 0",
			eventType: corev1.EventTypeNormal, reason: "UpgradeCheckCompleted",
			expect: "nova-status upgrade check exit 0",
		},
		{
			name: "warnings", message: "nova-status upgrade check exit 1",
			eventType: corev1.EventTypeWarning, reason: "UpgradeCheckWarnings",
			expect: "nova-status upgrade check exit 1",
		},
		{
			name: "a pod that wrote no message", message: "",
			eventType: corev1.EventTypeNormal, reason: "UpgradeCheckCompleted",
			expect: "exit code unavailable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			nova := validNova()
			observed := terminalJob(upgradeExpandJobSuffix, "expand-uid-"+tc.name)
			r := newNovaTestReconciler(nova, terminatedJobPod(observed.Name, tc.message))

			r.recordUpgradePhaseTerminal(context.Background(), r.Client, nova, upgradeExpandJobSuffix, observed)

			events := collectEvents(r.Recorder.(*record.FakeRecorder))
			g.Expect(events).To(ConsistOf(ContainSubstring(tc.eventType + " " + tc.reason)))
			g.Expect(events[0]).To(ContainSubstring(tc.expect))
			g.Expect(nova.Annotations).To(HaveKey(dbJobUIDAnnotationKey(upgradeExpandJobSuffix)),
				"the phase metric is emitted alongside the check report")
		})
	}

	// The check runs in the expand phase, so no other phase reports one.
	t.Run("the migrate phase emits no check event", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := validNova()
		observed := terminalJob(upgradeMigrateJobSuffix, "migrate-uid")
		r := newNovaTestReconciler(nova, terminatedJobPod(observed.Name, "nova-status upgrade check exit 0"))

		r.recordUpgradePhaseTerminal(context.Background(), r.Client, nova, upgradeMigrateJobSuffix, observed)

		g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(BeEmpty())
		g.Expect(nova.Annotations).To(HaveKey(dbJobUIDAnnotationKey(upgradeMigrateJobSuffix)))
	})

	// The dedupe is what keeps a requeue loop from re-reporting the same Job.
	t.Run("one event per Job UID", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := validNova()
		observed := terminalJob(upgradeExpandJobSuffix, "expand-uid-dedupe")
		r := newNovaTestReconciler(nova, terminatedJobPod(observed.Name, "nova-status upgrade check exit 0"))

		r.recordUpgradePhaseTerminal(context.Background(), r.Client, nova, upgradeExpandJobSuffix, observed)
		r.recordUpgradePhaseTerminal(context.Background(), r.Client, nova, upgradeExpandJobSuffix, observed)

		g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(HaveLen(1))
	})
}

// TestNovaJobs_PodSettings pins the pod settings of the db-sync Job, the upgrade phases and the db-archive CronJob: unset, every
// container renders the Job resources and no priority class or placement; the
// API Deployment's priority class and node selector carry over; spec.jobs
// overrides both.
func TestNovaJobs_PodSettings(t *testing.T) {
	podSpecs := func(o *novav1alpha1.Nova) map[string]corev1.PodSpec {
		return map[string]corev1.PodSpec{
			"db-sync":    database.SyncJob(novaJobSetParams(o, testConfigMapName)).Spec.Template.Spec,
			"db-expand":  database.BuildJob(novaJobSetParams(o, testConfigMapName), o.Spec.Image.Reference(), "db-expand", nil, 4).Spec.Template.Spec,
			"db-archive": dbArchiveCronJob(o, workloadArtifacts()).Spec.JobTemplate.Spec.Template.Spec,
		}
	}
	defaults := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("368Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("368Mi")},
	}

	for _, tc := range []struct {
		name         string
		mutate       func(o *novav1alpha1.Nova)
		wantPriority string
		wantSelector map[string]string
	}{
		{name: "defaults", mutate: func(*novav1alpha1.Nova) {}},
		{
			name: "API Deployment fallback",
			mutate: func(o *novav1alpha1.Nova) {
				o.Spec.API.Deployment.PriorityClassName = ptr.To("high")
				o.Spec.API.Deployment.NodeSelector = map[string]string{"a": "b"}
			},
			wantPriority: "high",
			wantSelector: map[string]string{"a": "b"},
		},
		{
			name: "spec.jobs override",
			mutate: func(o *novav1alpha1.Nova) {
				o.Spec.API.Deployment.PriorityClassName = ptr.To("high")
				o.Spec.API.Deployment.NodeSelector = map[string]string{"a": "b"}
				o.Spec.Jobs = &commonv1.JobSpec{
					JobBaseSpec:       commonv1.JobBaseSpec{PriorityClassName: ptr.To("low")},
					NodePlacementSpec: commonv1.NodePlacementSpec{NodeSelector: map[string]string{"pool": "jobs"}},
				}
			},
			wantPriority: "low",
			wantSelector: map[string]string{"pool": "jobs"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			o := defaulted(validNova())
			tc.mutate(o)

			for name, spec := range podSpecs(o) {
				g.Expect(spec.PriorityClassName).To(Equal(tc.wantPriority), name)
				g.Expect(spec.NodeSelector).To(Equal(tc.wantSelector), name)
				for _, c := range append(spec.InitContainers, spec.Containers...) {
					g.Expect(c.Resources).To(Equal(defaults), "%s/%s", name, c.Name)
				}
			}
		})
	}
}
