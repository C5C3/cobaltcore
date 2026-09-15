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

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/job"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

const (
	flowInstance  = "keystone"
	flowNamespace = "openstack"
)

func managedDBSpec() *commonv1.DatabaseSpec {
	return &commonv1.DatabaseSpec{
		ClusterRef: &corev1.LocalObjectReference{Name: "mariadb"},
		Database:   "keystone",
		SecretRef:  commonv1.SecretRefSpec{Name: "keystone-db"},
	}
}

// flowOwner is an owner CR in the flow namespace so SetControllerReference on
// the provisioned/owned resources does not trip the cross-namespace guard.
func flowOwner() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-owner", Namespace: flowNamespace, UID: "flow-uid"},
	}
}

func readyMariaDB() *mariadbv1alpha1.MariaDB {
	m := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "mariadb", Namespace: flowNamespace},
	}
	meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "Running",
	})
	return m
}

func readyDatabaseCR() *mariadbv1alpha1.Database {
	return readyDatabaseNamed(flowInstance)
}

func flowScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = mariadbv1alpha1.AddToScheme(s)
	_ = batchv1.AddToScheme(s)
	return s
}

func provisionParamsFor(spec *commonv1.DatabaseSpec, conds *[]metav1.Condition, owner client.Object) ProvisionFlowParams {
	return ProvisionFlowParams{
		Client:        nil, // set by caller
		Scheme:        nil,
		Owner:         owner,
		InstanceName:  flowInstance,
		Namespace:     flowNamespace,
		Database:      spec,
		Conditions:    conds,
		Generation:    1,
		ConditionType: "DatabaseReady",
		RequeueAfter:  30 * time.Second,
	}
}

// cellDBSpec is the Nova cell block's DatabaseSpec: the primary nova schema on
// the nova SQL user.
func cellDBSpec() *commonv1.DatabaseSpec {
	return &commonv1.DatabaseSpec{
		ClusterRef: &corev1.LocalObjectReference{Name: "mariadb"},
		Database:   "nova",
		SecretRef:  commonv1.SecretRefSpec{Name: "nova-db"},
	}
}

// cellProvisionParams is provisionParamsFor with the Nova cell block's instance
// name and its one additional schema.
func cellProvisionParams(spec *commonv1.DatabaseSpec, conds *[]metav1.Condition, owner client.Object) ProvisionFlowParams {
	p := provisionParamsFor(spec, conds, owner)
	p.InstanceName = "nova"
	p.AdditionalDatabaseNames = []string{"nova_cell0"}
	return p
}

func readyDatabaseNamed(name string) *mariadbv1alpha1.Database {
	db := &mariadbv1alpha1.Database{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: flowNamespace}}
	meta.SetStatusCondition(&db.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "Created",
	})
	return db
}

func readyUserNamed(name string) *mariadbv1alpha1.User {
	user := &mariadbv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: flowNamespace}}
	meta.SetStatusCondition(&user.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "Created",
	})
	return user
}

func readyGrantNamed(name string) *mariadbv1alpha1.Grant {
	grant := &mariadbv1alpha1.Grant{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: flowNamespace}}
	meta.SetStatusCondition(&grant.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "Created",
	})
	return grant
}

// applyRecorder records every applied resource as "<Kind>/<name>" in *applied
// and fails the apply of failKind/failName with boom. The ensure path writes
// through Server-Side Apply rather than Create, so Apply is the hook that
// observes the provisioning order.
func applyRecorder(applied *[]string, failKind, failName string, boom error) interceptor.Funcs {
	return interceptor.Funcs{
		Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration,
			opts ...client.ApplyOption,
		) error {
			co, ok := obj.(client.Object)
			if !ok {
				return cl.Apply(ctx, obj, opts...)
			}
			kind := co.GetObjectKind().GroupVersionKind().Kind
			*applied = append(*applied, kind+"/"+co.GetName())
			if kind == failKind && co.GetName() == failName {
				return boom
			}
			return cl.Apply(ctx, obj, opts...)
		},
	}
}

// --- IsClusterReady ---

func TestIsClusterReady(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	spec := managedDBSpec()
	ctx := context.Background()

	// Absent cluster -> false, nil.
	c := fake.NewClientBuilder().WithScheme(s).Build()
	ready, err := IsClusterReady(ctx, c, spec, flowNamespace)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())

	// Not-ready cluster -> false, nil.
	notReady := &mariadbv1alpha1.MariaDB{ObjectMeta: metav1.ObjectMeta{Name: "mariadb", Namespace: flowNamespace}}
	c = fake.NewClientBuilder().WithScheme(s).WithObjects(notReady).Build()
	ready, err = IsClusterReady(ctx, c, spec, flowNamespace)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())

	// Ready cluster -> true, nil.
	c = fake.NewClientBuilder().WithScheme(s).WithObjects(readyMariaDB()).Build()
	ready, err = IsClusterReady(ctx, c, spec, flowNamespace)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())
}

func TestIsClusterReady_transientGetError(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	boom := errors.New("apiserver unavailable")
	c := fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			return boom
		},
	}).Build()

	ready, err := IsClusterReady(context.Background(), c, managedDBSpec(), flowNamespace)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("apiserver unavailable"))
	g.Expect(ready).To(BeFalse())
}

// --- ReconcileProvision ---

func TestReconcileProvision_brownfieldNoOp(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := flowOwner()
	var conds []metav1.Condition
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()

	spec := &commonv1.DatabaseSpec{Host: "db.example.com", Database: "keystone", SecretRef: commonv1.SecretRefSpec{Name: "keystone-db"}}
	p := provisionParamsFor(spec, &conds, owner)
	p.Client, p.Scheme = c, s

	res, err := ReconcileProvision(context.Background(), p)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	// No condition is set on the brownfield no-op path.
	g.Expect(conds).To(BeEmpty())
}

func TestReconcileProvision_clusterNotReady(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := flowOwner()
	var conds []metav1.Condition
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()

	p := provisionParamsFor(managedDBSpec(), &conds, owner)
	p.Client, p.Scheme = c, s

	res, err := ReconcileProvision(context.Background(), p)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(30 * time.Second))
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal(ReasonClusterNotReady))
	g.Expect(cond.Message).To(Equal(`MariaDB cluster "mariadb" is not ready`))
}

func TestReconcileProvision_databaseNotReady(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := flowOwner()
	var conds []metav1.Condition
	// Cluster Ready but no Database CR yet: EnsureDatabase applies it and reports
	// not-ready.
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner, readyMariaDB()).Build()

	p := provisionParamsFor(managedDBSpec(), &conds, owner)
	p.Client, p.Scheme = c, s

	res, err := ReconcileProvision(context.Background(), p)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(30 * time.Second))
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(ReasonWaitingForDatabase))
	g.Expect(cond.Message).To(Equal("MariaDB Database CR is not ready"))
}

func TestReconcileProvision_staticWaitsForUser(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := flowOwner()
	var conds []metav1.Condition
	// Cluster + Database Ready, no User/Grant: Static mode must wait on them.
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, readyMariaDB(), readyDatabaseCR()).
		WithStatusSubresource(readyDatabaseCR()).
		Build()

	p := provisionParamsFor(managedDBSpec(), &conds, owner)
	p.Client, p.Scheme = c, s

	res, err := ReconcileProvision(context.Background(), p)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(30 * time.Second))
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(ReasonWaitingForDatabase))
	g.Expect(cond.Message).To(Equal("MariaDB User or Grant CR is not ready"))
}

// MaxUserConnections travels from the flow params into the applied User CR, so
// an operator sizing the cap from its CR topology reaches the SQL user through
// one field instead of re-building the User itself.
func TestReconcileProvision_forwardsMaxUserConnections(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := flowOwner()
	var conds []metav1.Condition
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, readyMariaDB(), readyDatabaseCR()).
		WithStatusSubresource(readyDatabaseCR()).
		Build()

	p := provisionParamsFor(managedDBSpec(), &conds, owner)
	p.Client, p.Scheme = c, s
	p.MaxUserConnections = 18

	_, err := ReconcileProvision(context.Background(), p)
	g.Expect(err).NotTo(HaveOccurred())

	user := &mariadbv1alpha1.User{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: flowInstance, Namespace: flowNamespace}, user)).To(Succeed())
	g.Expect(user.Spec.MaxUserConnections).To(Equal(int32(18)))
}

func TestReconcileProvision_dynamicSkipsUser(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := flowOwner()
	var conds []metav1.Condition
	// Cluster + Database Ready, Dynamic mode: the User/Grant are engine-owned, so
	// provisioning is complete without them.
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, readyMariaDB(), readyDatabaseCR()).
		WithStatusSubresource(readyDatabaseCR()).
		Build()

	spec := managedDBSpec()
	spec.CredentialsMode = commonv1.CredentialsModeDynamic
	p := provisionParamsFor(spec, &conds, owner)
	p.Client, p.Scheme = c, s

	res, err := ReconcileProvision(context.Background(), p)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	// No User/Grant CR was created.
	users := &mariadbv1alpha1.UserList{}
	g.Expect(c.List(context.Background(), users)).To(Succeed())
	g.Expect(users.Items).To(BeEmpty())
}

// A block without additional schemas provisions exactly the three resources it
// always did: both additional loops are no-ops on an empty slice.
func TestReconcileProvision_noAdditionalIsUnchanged(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := flowOwner()
	var conds []metav1.Condition
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, readyMariaDB(), readyDatabaseCR(), readyUserNamed(flowInstance), readyGrantNamed(flowInstance)).
		WithStatusSubresource(readyDatabaseCR(), readyUserNamed(flowInstance), readyGrantNamed(flowInstance)).
		Build()

	p := provisionParamsFor(managedDBSpec(), &conds, owner)
	p.Client, p.Scheme = c, s

	res, err := ReconcileProvision(context.Background(), p)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	dbs := &mariadbv1alpha1.DatabaseList{}
	g.Expect(c.List(context.Background(), dbs, client.InNamespace(flowNamespace))).To(Succeed())
	g.Expect(dbs.Items).To(HaveLen(1))
	g.Expect(dbs.Items[0].Name).To(Equal(flowInstance))
	grants := &mariadbv1alpha1.GrantList{}
	g.Expect(c.List(context.Background(), grants, client.InNamespace(flowNamespace))).To(Succeed())
	g.Expect(grants.Items).To(HaveLen(1))
	g.Expect(grants.Items[0].Name).To(Equal(flowInstance))
}

// The ordering contract: every schema exists before the user is applied, and
// every additional Grant follows the user and the primary Grant.
func TestReconcileProvision_additionalSchemasStatic(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := flowOwner()
	var conds []metav1.Condition
	var applied []string
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, readyMariaDB(),
			readyDatabaseNamed("nova"), readyDatabaseNamed("nova-nova-cell0"),
			readyUserNamed("nova"), readyGrantNamed("nova"), readyGrantNamed("nova-nova-cell0")).
		WithStatusSubresource(
			readyDatabaseNamed("nova"), readyDatabaseNamed("nova-nova-cell0"),
			readyUserNamed("nova"), readyGrantNamed("nova"), readyGrantNamed("nova-nova-cell0")).
		WithInterceptorFuncs(applyRecorder(&applied, "", "", nil)).
		Build()

	p := cellProvisionParams(cellDBSpec(), &conds, owner)
	p.Client, p.Scheme = c, s

	res, err := ReconcileProvision(context.Background(), p)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(applied).To(Equal([]string{
		"Database/nova", "Database/nova-nova-cell0",
		"User/nova", "Grant/nova", "Grant/nova-nova-cell0",
	}))
}

// In Dynamic mode the engine role's creation_statements carry the grants of the
// additional schemas too, so only their Databases are provisioned.
func TestReconcileProvision_dynamicSkipsAdditionalGrants(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := flowOwner()
	var conds []metav1.Condition
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, readyMariaDB(), readyDatabaseNamed("nova"), readyDatabaseNamed("nova-nova-cell0")).
		WithStatusSubresource(readyDatabaseNamed("nova"), readyDatabaseNamed("nova-nova-cell0")).
		Build()

	spec := cellDBSpec()
	spec.CredentialsMode = commonv1.CredentialsModeDynamic
	p := cellProvisionParams(spec, &conds, owner)
	p.Client, p.Scheme = c, s

	res, err := ReconcileProvision(context.Background(), p)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	grants := &mariadbv1alpha1.GrantList{}
	g.Expect(c.List(context.Background(), grants, client.InNamespace(flowNamespace))).To(Succeed())
	g.Expect(grants.Items).To(BeEmpty())
	users := &mariadbv1alpha1.UserList{}
	g.Expect(c.List(context.Background(), users, client.InNamespace(flowNamespace))).To(Succeed())
	g.Expect(users.Items).To(BeEmpty())
	// The additional schema is still operator-managed.
	db := &mariadbv1alpha1.Database{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "nova-nova-cell0", Namespace: flowNamespace}, db)).To(Succeed())
}

// An additional schema that is applied but not yet Ready parks the flow before
// the user, and names the schema in the condition message.
func TestReconcileProvision_additionalDatabaseNotReady(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := flowOwner()
	var conds []metav1.Condition
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, readyMariaDB(), readyDatabaseNamed("nova")).
		WithStatusSubresource(readyDatabaseNamed("nova")).
		Build()

	p := cellProvisionParams(cellDBSpec(), &conds, owner)
	p.Client, p.Scheme = c, s

	res, err := ReconcileProvision(context.Background(), p)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(30 * time.Second))
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(ReasonWaitingForDatabase))
	g.Expect(cond.Message).To(Equal(`MariaDB Database CR for schema "nova_cell0" is not ready`))
	// The Database was applied; only its readiness is outstanding.
	db := &mariadbv1alpha1.Database{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "nova-nova-cell0", Namespace: flowNamespace}, db)).To(Succeed())
}

// The additional Grant gates provisioning the same way the primary one does.
func TestReconcileProvision_additionalGrantNotReady(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := flowOwner()
	var conds []metav1.Condition
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, readyMariaDB(),
			readyDatabaseNamed("nova"), readyDatabaseNamed("nova-nova-cell0"),
			readyUserNamed("nova"), readyGrantNamed("nova")).
		WithStatusSubresource(
			readyDatabaseNamed("nova"), readyDatabaseNamed("nova-nova-cell0"),
			readyUserNamed("nova"), readyGrantNamed("nova")).
		Build()

	p := cellProvisionParams(cellDBSpec(), &conds, owner)
	p.Client, p.Scheme = c, s

	res, err := ReconcileProvision(context.Background(), p)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(30 * time.Second))
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(ReasonWaitingForDatabase))
	g.Expect(cond.Message).To(Equal(`MariaDB Grant CR for schema "nova_cell0" is not ready`))
	grant := &mariadbv1alpha1.Grant{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "nova-nova-cell0", Namespace: flowNamespace}, grant)).To(Succeed())
}

// An invalid additional-schema slice fails the flow before the first apply, and
// the error names its cause: an entry outside the schema identifier set, an
// entry that derives an invalid object name, the primary schema listed again (a
// second Database CR on one SQL schema), or two entries that derive one object
// name. The check runs before the brownfield return, so a brownfield block
// rejects the same slice.
func TestReconcileProvision_rejectsInvalidAdditional(t *testing.T) {
	cases := []struct {
		name       string
		spec       *commonv1.DatabaseSpec
		additional []string
		want       string
	}{
		{
			"primary schema listed again",
			cellDBSpec(),
			[]string{"nova"},
			`additional database "nova" equals the primary schema`,
		},
		{
			"same schema listed twice",
			cellDBSpec(),
			[]string{"nova_cell0", "nova_cell0"},
			`additional databases "nova_cell0" and "nova_cell0" both derive the object name "nova-nova-cell0"`,
		},
		{
			"schemas differing only in case",
			cellDBSpec(),
			[]string{"nova_cell0", "Nova_Cell0"},
			`additional databases "nova_cell0" and "Nova_Cell0" both derive the object name "nova-nova-cell0"`,
		},
		{
			"schema outside the identifier set",
			cellDBSpec(),
			[]string{"nova-cell0"},
			`additional database "nova-cell0" must match ^[A-Za-z0-9_]{1,64}$`,
		},
		{
			// The pattern admits a trailing underscore, but the derived name then
			// ends in a hyphen and no API server accepts it.
			"schema deriving an invalid object name",
			cellDBSpec(),
			[]string{"nova_cell0_"},
			`additional database "nova_cell0_" derives the invalid object name "nova-nova-cell0-": ` +
				`a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', ` +
				`and must start and end with an alphanumeric character (e.g. 'example.com', ` +
				`regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')`,
		},
		{
			"primary schema listed again on a brownfield block",
			&commonv1.DatabaseSpec{Host: "db.example.com", Database: "nova", SecretRef: commonv1.SecretRefSpec{Name: "nova-db"}},
			[]string{"nova"},
			`additional database "nova" equals the primary schema`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			s := flowScheme()
			owner := flowOwner()
			var conds []metav1.Condition
			// The cluster is Ready, so an unvalidated slice would reach the apply.
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner, readyMariaDB()).Build()

			p := cellProvisionParams(tc.spec, &conds, owner)
			p.Client, p.Scheme = c, s
			p.AdditionalDatabaseNames = tc.additional

			res, err := ReconcileProvision(context.Background(), p)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(Equal(tc.want))
			g.Expect(res.IsZero()).To(BeTrue())
			dbs := &mariadbv1alpha1.DatabaseList{}
			g.Expect(c.List(context.Background(), dbs, client.InNamespace(flowNamespace))).To(Succeed())
			g.Expect(dbs.Items).To(BeEmpty())
			g.Expect(conds).To(BeEmpty())
		})
	}
}

// An empty schema name would derive the object name "nova-", which no API
// server accepts, so it is rejected before the first apply as well.
func TestReconcileProvision_rejectsEmptyAdditional(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := flowOwner()
	var conds []metav1.Condition
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner, readyMariaDB()).Build()

	p := cellProvisionParams(cellDBSpec(), &conds, owner)
	p.Client, p.Scheme = c, s
	p.AdditionalDatabaseNames = []string{""}

	res, err := ReconcileProvision(context.Background(), p)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(Equal("additional database name must not be empty"))
	g.Expect(res.IsZero()).To(BeTrue())
	dbs := &mariadbv1alpha1.DatabaseList{}
	g.Expect(c.List(context.Background(), dbs, client.InNamespace(flowNamespace))).To(Succeed())
	g.Expect(dbs.Items).To(BeEmpty())
	g.Expect(conds).To(BeEmpty())
}

// A failing apply of an additional Database surfaces as a hard error naming the
// schema, not as a requeue.
func TestReconcileProvision_additionalDatabaseError(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := flowOwner()
	var conds []metav1.Condition
	var applied []string
	boom := errors.New("apply rejected")
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, readyMariaDB(), readyDatabaseNamed("nova")).
		WithStatusSubresource(readyDatabaseNamed("nova")).
		WithInterceptorFuncs(applyRecorder(&applied, "Database", "nova-nova-cell0", boom)).
		Build()

	p := cellProvisionParams(cellDBSpec(), &conds, owner)
	p.Client, p.Scheme = c, s

	_, err := ReconcileProvision(context.Background(), p)
	g.Expect(errors.Is(err, boom)).To(BeTrue())
	g.Expect(err.Error()).To(HavePrefix(`ensuring additional Database "nova_cell0": `))
	// The flow stops at the failed apply: no User or Grant follows it.
	g.Expect(applied).To(Equal([]string{"Database/nova", "Database/nova-nova-cell0"}))
}

// The same for a failing apply of an additional Grant.
func TestReconcileProvision_additionalGrantError(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := flowOwner()
	var conds []metav1.Condition
	var applied []string
	boom := errors.New("apply rejected")
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, readyMariaDB(),
			readyDatabaseNamed("nova"), readyDatabaseNamed("nova-nova-cell0"),
			readyUserNamed("nova"), readyGrantNamed("nova")).
		WithStatusSubresource(
			readyDatabaseNamed("nova"), readyDatabaseNamed("nova-nova-cell0"),
			readyUserNamed("nova"), readyGrantNamed("nova")).
		WithInterceptorFuncs(applyRecorder(&applied, "Grant", "nova-nova-cell0", boom)).
		Build()

	p := cellProvisionParams(cellDBSpec(), &conds, owner)
	p.Client, p.Scheme = c, s

	_, err := ReconcileProvision(context.Background(), p)
	g.Expect(errors.Is(err, boom)).To(BeTrue())
	g.Expect(err.Error()).To(HavePrefix(`ensuring additional Grant "nova_cell0": `))
	g.Expect(applied).To(Equal([]string{
		"Database/nova", "Database/nova-nova-cell0",
		"User/nova", "Grant/nova", "Grant/nova-nova-cell0",
	}))
}

// --- ReconcileSyncJobs ---

// syncParams builds a keystone-shaped SyncFlowParams against c, recording every
// terminal callback into *calls.
func syncParams(c client.Client, s *runtime.Scheme, owner client.Object, conds *[]metav1.Condition, rec record.EventRecorder, installed *string, calls *[]string) SyncFlowParams {
	return SyncFlowParams{
		Client:   c,
		Scheme:   s,
		Recorder: rec,
		Owner:    owner,
		Jobs:     keystoneJobSet(),
		RecordTerminal: func(jobSuffix string, _ *batchv1.Job) {
			*calls = append(*calls, jobSuffix)
		},
		Conditions:       conds,
		Generation:       1,
		ConditionType:    "DatabaseReady",
		RequeueAfter:     30 * time.Second,
		InstalledRelease: installed,
		ImageTag:         "2026.1",
	}
}

func completedJob(name string, hash string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: flowNamespace,
			Annotations: map[string]string{job.PodSpecHashAnnotation: hash},
		},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
		}},
	}
}

func failedJob(name string, hash string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: flowNamespace,
			Annotations: map[string]string{job.PodSpecHashAnnotation: hash},
		},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"},
		}},
	}
}

func TestReconcileSyncJobs_dbSyncInProgress(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := flowOwner()
	var conds []metav1.Condition
	var calls []string
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()

	res, err := ReconcileSyncJobs(context.Background(), syncParams(c, s, owner, &conds, nil, nil, &calls))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(30 * time.Second))
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(ReasonDBSyncInProgress))
	// The db-sync Job was created and the terminal callback observed it.
	g.Expect(calls).To(Equal([]string{"db-sync"}))
	created := &batchv1.Job{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "keystone-db-sync", Namespace: flowNamespace}, created)).To(Succeed())
}

func TestReconcileSyncJobs_schemaCheckSequenced(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := flowOwner()
	var conds []metav1.Condition
	var calls []string
	p := keystoneJobSet()
	syncHash := job.PodSpecHash(&SyncJob(p).Spec.Template)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, completedJob("keystone-db-sync", syncHash)).
		Build()

	res, err := ReconcileSyncJobs(context.Background(), syncParams(c, s, owner, &conds, nil, nil, &calls))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(30 * time.Second))
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(ReasonSchemaCheckInProgress))
	// db-sync observed (complete) then schema-check created.
	g.Expect(calls).To(Equal([]string{"db-sync", "schema-check"}))
	created := &batchv1.Job{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "keystone-schema-check", Namespace: flowNamespace}, created)).To(Succeed())
}

func TestReconcileSyncJobs_success(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := flowOwner()
	var conds []metav1.Condition
	var calls []string
	rec := record.NewFakeRecorder(10)
	installed := ""
	p := keystoneJobSet()
	syncHash := job.PodSpecHash(&SyncJob(p).Spec.Template)
	checkHash := job.PodSpecHash(&SchemaCheckJob(p).Spec.Template)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(
			owner,
			completedJob("keystone-db-sync", syncHash),
			completedJob("keystone-schema-check", checkHash),
		).Build()

	res, err := ReconcileSyncJobs(context.Background(), syncParams(c, s, owner, &conds, rec, &installed, &calls))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(ReasonDatabaseSynced))
	// InstalledRelease is promoted to the image tag on success.
	g.Expect(installed).To(Equal("2026.1"))
	g.Expect(rec.Events).To(Receive(ContainSubstring(ReasonDatabaseSynced)))
}

func TestReconcileSyncJobs_dbSyncFailed(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := flowOwner()
	var conds []metav1.Condition
	var calls []string
	rec := record.NewFakeRecorder(10)
	installed := ""
	p := keystoneJobSet()
	syncHash := job.PodSpecHash(&SyncJob(p).Spec.Template)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, failedJob("keystone-db-sync", syncHash)).
		Build()

	res, err := ReconcileSyncJobs(context.Background(), syncParams(c, s, owner, &conds, rec, &installed, &calls))
	g.Expect(err).To(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(ReasonDBSyncFailed))
	// InstalledRelease is NOT promoted on failure.
	g.Expect(installed).To(BeEmpty())
	g.Expect(rec.Events).To(Receive(ContainSubstring(ReasonDBSyncFailed)))
}

// TestReconcileSyncJobs_noSchemaCheck exercises the nil-SchemaCheckCommand edge
// path: a service without a schema-check step reaches DatabaseSynced straight off
// a completed db-sync, and no schema-check Job is created.
func TestReconcileSyncJobs_noSchemaCheck(t *testing.T) {
	g := NewWithT(t)
	s := flowScheme()
	owner := flowOwner()
	var conds []metav1.Condition
	var calls []string
	p := keystoneJobSet()
	p.SchemaCheckCommand = nil
	syncHash := job.PodSpecHash(&SyncJob(p).Spec.Template)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(owner, completedJob("keystone-db-sync", syncHash)).
		Build()

	params := syncParams(c, s, owner, &conds, nil, nil, &calls)
	params.Jobs = p
	res, err := ReconcileSyncJobs(context.Background(), params)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := meta.FindStatusCondition(conds, "DatabaseReady")
	g.Expect(cond.Reason).To(Equal(ReasonDatabaseSynced))
	g.Expect(calls).To(Equal([]string{"db-sync"}))
	// No schema-check Job was created.
	check := &batchv1.Job{}
	err = c.Get(context.Background(), client.ObjectKey{Name: "keystone-schema-check", Namespace: flowNamespace}, check)
	g.Expect(err).To(HaveOccurred())
}
