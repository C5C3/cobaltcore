// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package validation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

var testPath = field.NewPath("spec", "test")

func TestDatabaseXOR(t *testing.T) {
	cases := []struct {
		name    string
		db      commonv1.DatabaseSpec
		wantErr bool
	}{
		{"managed mode valid", commonv1.DatabaseSpec{ClusterRef: &corev1.LocalObjectReference{Name: "db"}}, false},
		{"brownfield mode valid", commonv1.DatabaseSpec{Host: "db.example.com"}, false},
		{"both set rejected", commonv1.DatabaseSpec{ClusterRef: &corev1.LocalObjectReference{Name: "db"}, Host: "db.example.com"}, true},
		{"neither set rejected", commonv1.DatabaseSpec{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			errs := DatabaseXOR(testPath, &tc.db)
			g.Expect(len(errs) > 0).To(gomega.Equal(tc.wantErr))
		})
	}
}

func TestCacheXOR(t *testing.T) {
	cases := []struct {
		name    string
		cache   commonv1.CacheSpec
		wantErr bool
	}{
		{"managed mode valid", commonv1.CacheSpec{ClusterRef: &corev1.LocalObjectReference{Name: "mc"}}, false},
		{"brownfield mode valid", commonv1.CacheSpec{Servers: []string{"mc-0:11211"}}, false},
		{"both set rejected", commonv1.CacheSpec{ClusterRef: &corev1.LocalObjectReference{Name: "mc"}, Servers: []string{"mc-0:11211"}}, true},
		// An explicitly empty servers list is NOT a brownfield configuration:
		// with no clusterRef either, the spec has no usable cache source.
		{"neither set rejected", commonv1.CacheSpec{Servers: []string{}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			errs := CacheXOR(testPath, &tc.cache)
			g.Expect(len(errs) > 0).To(gomega.Equal(tc.wantErr))
		})
	}
}

func TestMessagingXOR(t *testing.T) {
	cases := []struct {
		name      string
		messaging commonv1.MessagingSpec
		wantErr   bool
	}{
		{"managed mode valid", commonv1.MessagingSpec{ClusterRef: &corev1.LocalObjectReference{Name: "rabbitmq"}}, false},
		{"brownfield mode valid", commonv1.MessagingSpec{SecretRef: &commonv1.SecretRefSpec{Name: "transport-url"}}, false},
		{
			"both set rejected",
			commonv1.MessagingSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "rabbitmq"},
				SecretRef:  &commonv1.SecretRefSpec{Name: "transport-url"},
			},
			true,
		},
		{"neither set rejected", commonv1.MessagingSpec{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			errs := MessagingXOR(testPath, &tc.messaging)
			if !tc.wantErr {
				g.Expect(errs).To(gomega.BeEmpty())
				return
			}
			g.Expect(errs).To(gomega.HaveLen(1))
			g.Expect(errs[0].Field).To(gomega.Equal(testPath.String()))
			g.Expect(errs[0].Error()).To(gomega.ContainSubstring("exactly one of clusterRef or secretRef must be set"))
		})
	}
}

func TestCacheNoControlChars(t *testing.T) {
	cases := []struct {
		name    string
		cache   commonv1.CacheSpec
		wantErr bool
	}{
		{"managed mode clean", commonv1.CacheSpec{ClusterRef: &corev1.LocalObjectReference{Name: "mc"}}, false},
		{"brownfield mode clean", commonv1.CacheSpec{Servers: []string{"mc-0:11211", "mc-1:11211"}}, false},
		// Both shapes end up in [keystone_authtoken].memcached_servers, so a
		// newline smuggles an attacker-controlled auth_url into the section.
		{
			"server with a newline rejected",
			commonv1.CacheSpec{Servers: []string{"mc-0:11211\nauth_url = http://attacker.example/v3"}},
			true,
		},
		{
			"server with a carriage return rejected",
			commonv1.CacheSpec{Servers: []string{"mc-0:11211", "mc-1:11211\rregion_name = elsewhere"}},
			true,
		},
		{
			"clusterRef name with a newline rejected",
			commonv1.CacheSpec{ClusterRef: &corev1.LocalObjectReference{Name: "mc\nauth_url = http://attacker.example/v3"}},
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			errs := CacheNoControlChars(testPath, &tc.cache)
			g.Expect(len(errs) > 0).To(gomega.Equal(tc.wantErr))
		})
	}
}

func TestDynamicCredentialsRequireClusterRef(t *testing.T) {
	g := gomega.NewWithT(t)

	dynamicBrownfield := commonv1.DatabaseSpec{CredentialsMode: commonv1.CredentialsModeDynamic, Host: "db"}
	errs := DynamicCredentialsRequireClusterRef(testPath, &dynamicBrownfield)
	g.Expect(errs).To(gomega.HaveLen(1))
	g.Expect(errs[0].Field).To(gomega.Equal("spec.test.credentialsMode"))

	dynamicManaged := commonv1.DatabaseSpec{CredentialsMode: commonv1.CredentialsModeDynamic, ClusterRef: &corev1.LocalObjectReference{Name: "db"}}
	g.Expect(DynamicCredentialsRequireClusterRef(testPath, &dynamicManaged)).To(gomega.BeEmpty())

	staticBrownfield := commonv1.DatabaseSpec{CredentialsMode: commonv1.CredentialsModeStatic, Host: "db"}
	g.Expect(DynamicCredentialsRequireClusterRef(testPath, &staticBrownfield)).To(gomega.BeEmpty())
}

func TestCronSchedule(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(CronSchedule(testPath, "0 0 * * 0")).To(gomega.BeEmpty())
	g.Expect(CronSchedule(testPath, "@hourly")).To(gomega.BeEmpty())

	errs := CronSchedule(testPath, "not-a-cron")
	g.Expect(errs).To(gomega.HaveLen(1))
	g.Expect(errs[0].Detail).To(gomega.ContainSubstring("invalid cron expression"))

	// An empty schedule is a parse error here — the required-vs-defaulted
	// decision (and its message) is the caller's per-field policy.
	g.Expect(CronSchedule(testPath, "")).To(gomega.HaveLen(1))
}

func TestTopologySpreadSelector(t *testing.T) {
	required := map[string]string{
		"app.kubernetes.io/name":     "keystone",
		"app.kubernetes.io/instance": "ks",
	}
	tsc := func(sel *metav1.LabelSelector) corev1.TopologySpreadConstraint {
		return corev1.TopologySpreadConstraint{
			MaxSkew:           1,
			TopologyKey:       "kubernetes.io/hostname",
			WhenUnsatisfiable: corev1.ScheduleAnyway,
			LabelSelector:     sel,
		}
	}

	cases := []struct {
		name     string
		tscs     []corev1.TopologySpreadConstraint
		wantErrs int
	}{
		{"matching selector valid", []corev1.TopologySpreadConstraint{
			tsc(&metav1.LabelSelector{MatchLabels: required}),
		}, 0},
		{"missing selector rejected", []corev1.TopologySpreadConstraint{tsc(nil)}, 1},
		{"wrong labels rejected", []corev1.TopologySpreadConstraint{
			tsc(&metav1.LabelSelector{MatchLabels: map[string]string{"app": "other"}}),
		}, 1},
		{"matchExpressions rejected", []corev1.TopologySpreadConstraint{
			tsc(&metav1.LabelSelector{
				MatchLabels: required,
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "zone", Operator: metav1.LabelSelectorOpExists},
				},
			}),
		}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(TopologySpreadSelector(testPath, tc.tscs, required)).To(gomega.HaveLen(tc.wantErrs))
		})
	}
}

func TestPriorityClassExists(t *testing.T) {
	g := gomega.NewWithT(t)

	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithObjects(&schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: "critical"}}).
		Build()

	g.Expect(PriorityClassExists(context.Background(), c, testPath, "critical")).To(gomega.BeEmpty())

	errs := PriorityClassExists(context.Background(), c, testPath, "typo")
	g.Expect(errs).To(gomega.HaveLen(1))
	g.Expect(errs[0].Type).To(gomega.Equal(field.ErrorTypeNotFound))

	// A nil Reader (programmatically constructed webhook without a client)
	// and an empty name both skip the lookup rather than failing closed.
	g.Expect(PriorityClassExists(context.Background(), nil, testPath, "critical")).To(gomega.BeEmpty())
	g.Expect(PriorityClassExists(context.Background(), c, testPath, "")).To(gomega.BeEmpty())
}

func TestSecretStoreRef(t *testing.T) {
	path := field.NewPath("spec", "secretStoreRef")
	cases := []struct {
		name     string
		ref      *commonv1.SecretStoreRefSpec
		wantErrs int
		wantSub  string
	}{
		{"nil is allowed", nil, 0, ""},
		{"valid cluster ref", &commonv1.SecretStoreRefSpec{Kind: commonv1.SecretStoreKindCluster, Name: "openbao-cluster-store"}, 0, ""},
		{"valid namespaced ref", &commonv1.SecretStoreRefSpec{Kind: commonv1.SecretStoreKindNamespaced, Name: "openbao-tenant-store"}, 0, ""},
		{"empty kind defaults, still valid", &commonv1.SecretStoreRefSpec{Name: "some-store"}, 0, ""},
		{"empty name is required", &commonv1.SecretStoreRefSpec{Kind: commonv1.SecretStoreKindNamespaced}, 1, "name"},
		{"unknown kind not supported", &commonv1.SecretStoreRefSpec{Kind: commonv1.SecretStoreRefKind("Bogus"), Name: "x"}, 1, "kind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			errs := SecretStoreRef(path, tc.ref)
			g.Expect(errs).To(gomega.HaveLen(tc.wantErrs))
			if tc.wantSub != "" {
				g.Expect(errs.ToAggregate().Error()).To(gomega.ContainSubstring(tc.wantSub))
			}
		})
	}
}

func TestTargetClusterRef(t *testing.T) {
	path := field.NewPath("spec", "targetClusterRef")
	cases := []struct {
		name     string
		ref      *commonv1.TargetClusterRefSpec
		wantErrs int
		wantSub  string
	}{
		{"nil selects the management cluster", nil, 0, ""},
		{"valid ref", &commonv1.TargetClusterRefSpec{Name: "edge-1"}, 0, ""},
		{"empty name is required", &commonv1.TargetClusterRefSpec{}, 1, "targetClusterRef.name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			errs := TargetClusterRef(path, tc.ref)
			g.Expect(errs).To(gomega.HaveLen(tc.wantErrs))
			if tc.wantSub != "" {
				g.Expect(errs.ToAggregate().Error()).To(gomega.ContainSubstring(tc.wantSub))
			}
		})
	}
}

func TestTargetClusterRefImmutable(t *testing.T) {
	path := field.NewPath("spec", "targetClusterRef")
	cases := []struct {
		name     string
		oldRef   *commonv1.TargetClusterRefSpec
		newRef   *commonv1.TargetClusterRefSpec
		wantErrs int
	}{
		{"both nil is unchanged", nil, nil, 0},
		{"same name is unchanged", &commonv1.TargetClusterRefSpec{Name: "edge-1"}, &commonv1.TargetClusterRefSpec{Name: "edge-1"}, 0},
		{"adding the ref rejected", nil, &commonv1.TargetClusterRefSpec{Name: "edge-1"}, 1},
		{"removing the ref rejected", &commonv1.TargetClusterRefSpec{Name: "edge-1"}, nil, 1},
		{"renaming the ref rejected", &commonv1.TargetClusterRefSpec{Name: "edge-1"}, &commonv1.TargetClusterRefSpec{Name: "edge-2"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			errs := TargetClusterRefImmutable(path, tc.oldRef, tc.newRef)
			g.Expect(errs).To(gomega.HaveLen(tc.wantErrs))
			if tc.wantErrs > 0 {
				g.Expect(errs.ToAggregate().Error()).To(gomega.ContainSubstring("targetClusterRef is immutable"))
			}
		})
	}
}

// The sibling validators are generic over client.Object, so the tests use a
// ConfigMap as the stand-in CR: annotation "parent" is the parent reference the
// sameParent predicate compares, and annotation "default" set to "true" is the
// default flag isDefault reads.
const (
	parentAnnotation  = "parent"
	defaultAnnotation = "default"
)

var errBoom = errors.New("boom")

func siblingCM(namespace, name, parent string, isDefault bool) *corev1.ConfigMap {
	annotations := map[string]string{parentAnnotation: parent}
	if isDefault {
		annotations[defaultAnnotation] = "true"
	}
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Namespace:   namespace,
		Name:        name,
		Annotations: annotations,
	}}
}

// terminating marks cm as deleting. The fake client refuses an object that
// carries a deletionTimestamp without a finalizer, so cm gets one.
func terminating(cm *corev1.ConfigMap) *corev1.ConfigMap {
	deleted := metav1.NewTime(time.Now())
	cm.DeletionTimestamp = &deleted
	cm.Finalizers = []string{"test"}
	return cm
}

func sameParentAs(self client.Object) func(client.Object) bool {
	return func(other client.Object) bool {
		return other.GetAnnotations()[parentAnnotation] == self.GetAnnotations()[parentAnnotation]
	}
}

// siblingFixture seeds a client with self, the one live same-parent sibling the
// filter keeps, and the three it drops: a Terminating one, one attached to
// another parent, and one in another namespace.
func siblingFixture(self *corev1.ConfigMap, extra ...client.Object) client.Client {
	objs := []client.Object{
		self,
		siblingCM("ops", "live-sibling", "glance", false),
		terminating(siblingCM("ops", "terminating-sibling", "glance", false)),
		siblingCM("ops", "other-parent", "glance-2", false),
		siblingCM("elsewhere", "other-namespace", "glance", false),
	}
	return fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithObjects(append(objs, extra...)...).Build()
}

// listErrorClient fails every List with errBoom, so the tests can assert that
// the validators hand the error back untouched.
func listErrorClient() client.Client {
	return fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return errBoom
			},
		}).Build()
}

// itemlessList is a client.ObjectList without an Items field, which is what
// apimeta.ExtractList rejects. No real CR list looks like this; it exists to
// reach the extract-error path behind a List that succeeds.
type itemlessList struct {
	metav1.TypeMeta
	metav1.ListMeta
}

func (l *itemlessList) DeepCopyObject() runtime.Object { return &itemlessList{} }

// emptyListClient succeeds every List without touching the list object, so the
// caller sees whatever list type it passed in.
func emptyListClient() client.Client {
	return fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return nil
			},
		}).Build()
}

func objectNames(objs []client.Object) []string {
	names := make([]string, 0, len(objs))
	for _, o := range objs {
		names = append(names, o.GetName())
	}
	return names
}

func TestAttachedSiblings(t *testing.T) {
	ctx := context.Background()
	self := siblingCM("ops", "self", "glance", false)

	t.Run("nil reader skips the lookup", func(t *testing.T) {
		g := gomega.NewWithT(t)
		siblings, err := AttachedSiblings(ctx, nil, self, &corev1.ConfigMapList{}, sameParentAs(self))
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(siblings).To(gomega.BeNil())
	})

	t.Run("keeps only the live same-parent sibling", func(t *testing.T) {
		g := gomega.NewWithT(t)
		siblings, err := AttachedSiblings(ctx, siblingFixture(self), self, &corev1.ConfigMapList{}, sameParentAs(self))
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(objectNames(siblings)).To(gomega.Equal([]string{"live-sibling"}))
	})

	t.Run("list error is returned unwrapped", func(t *testing.T) {
		g := gomega.NewWithT(t)
		siblings, err := AttachedSiblings(ctx, listErrorClient(), self, &corev1.ConfigMapList{}, sameParentAs(self))
		g.Expect(siblings).To(gomega.BeNil())
		g.Expect(err).To(gomega.MatchError("boom"))
		g.Expect(err).To(gomega.Equal(errBoom))
		g.Expect(errors.Is(err, errBoom)).To(gomega.BeTrue())
	})

	t.Run("a typed predicate returns typed siblings", func(t *testing.T) {
		g := gomega.NewWithT(t)
		siblings, err := AttachedSiblings(ctx, siblingFixture(self), self, &corev1.ConfigMapList{},
			func(other *corev1.ConfigMap) bool {
				return other.Annotations[parentAnnotation] == self.Annotations[parentAnnotation]
			})
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(siblings).To(gomega.HaveLen(1))
		g.Expect(siblings[0].Name).To(gomega.Equal("live-sibling"))
	})

	t.Run("extract error is returned unwrapped", func(t *testing.T) {
		g := gomega.NewWithT(t)
		siblings, err := AttachedSiblings(ctx, emptyListClient(), self, &itemlessList{}, sameParentAs(self))
		g.Expect(siblings).To(gomega.BeNil())
		g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("Items field")))
	})
}

func TestExtraOptions(t *testing.T) {
	const patternDetail = "option name must match ^[A-Za-z0-9_]+$ (letters, digits, and underscore)"
	const controlCharsDetail = "value must not contain newline or carriage-return characters"
	denylist := map[string]string{"denied": "spec.owner"}

	t.Run("nil and empty options are accepted", func(t *testing.T) {
		g := gomega.NewWithT(t)
		g.Expect(ExtraOptions(testPath, nil, ExtraOptionsRules{})).To(gomega.BeNil())
		g.Expect(ExtraOptions(testPath, map[string]string{}, ExtraOptionsRules{Denylist: denylist})).To(gomega.BeNil())
	})

	t.Run("every rejection fires once, in sorted key order", func(t *testing.T) {
		g := gomega.NewWithT(t)
		errs := ExtraOptions(testPath, map[string]string{
			"":        "x",
			"bad key": "x",
			"denied":  "x",
			"ok":      "a\nb",
		}, ExtraOptionsRules{Denylist: denylist})
		g.Expect(errs).To(gomega.HaveLen(4))

		// The empty key has no path key to hang off, so it is reported on the
		// extraOptions path itself.
		g.Expect(errs[0].Field).To(gomega.Equal(testPath.String()))
		g.Expect(errs[0].BadValue).To(gomega.Equal(""))
		g.Expect(errs[0].Detail).To(gomega.Equal("option name must not be empty"))

		g.Expect(errs[1].Field).To(gomega.Equal(testPath.Key("bad key").String()))
		g.Expect(errs[1].BadValue).To(gomega.Equal("bad key"))
		g.Expect(errs[1].Detail).To(gomega.Equal(patternDetail))

		g.Expect(errs[2].Field).To(gomega.Equal(testPath.Key("denied").String()))
		g.Expect(errs[2].BadValue).To(gomega.Equal("x"))
		g.Expect(errs[2].Detail).To(gomega.Equal(`option "denied" is owned by spec.owner and must not be set via extraOptions`))

		g.Expect(errs[3].Field).To(gomega.Equal(testPath.Key("ok").String()))
		g.Expect(errs[3].BadValue).To(gomega.Equal("a\nb"))
		g.Expect(errs[3].Detail).To(gomega.Equal(controlCharsDetail))
	})

	t.Run("PerKey runs before the control-character check on the same key", func(t *testing.T) {
		g := gomega.NewWithT(t)
		errs := ExtraOptions(testPath, map[string]string{"ok": "a\nb"}, ExtraOptionsRules{
			PerKey: func(key, value string) *field.Error {
				return field.Invalid(testPath.Key(key), value, "per-key rule")
			},
		})
		g.Expect(errs).To(gomega.HaveLen(2))
		g.Expect(errs[0].Detail).To(gomega.Equal("per-key rule"))
		g.Expect(errs[1].Detail).To(gomega.Equal(controlCharsDetail))
	})

	t.Run("a denylisted key never reaches PerKey", func(t *testing.T) {
		g := gomega.NewWithT(t)
		var seen []string
		errs := ExtraOptions(testPath, map[string]string{"denied": "x"}, ExtraOptionsRules{
			Denylist: denylist,
			PerKey: func(key, _ string) *field.Error {
				seen = append(seen, key)
				return nil
			},
		})
		g.Expect(errs).To(gomega.HaveLen(1))
		g.Expect(seen).To(gomega.BeEmpty())
	})

	// The charset is not per-consumer: every caller gets
	// DefaultExtraOptionKeyPattern, which is the pattern the message names.
	t.Run("the default charset is enforced and is what the message names", func(t *testing.T) {
		g := gomega.NewWithT(t)
		errs := ExtraOptions(testPath, map[string]string{"with-dash": "x"}, ExtraOptionsRules{})
		g.Expect(errs).To(gomega.HaveLen(1))
		g.Expect(errs[0].Detail).To(gomega.Equal(patternDetail))
		g.Expect(patternDetail).To(gomega.ContainSubstring(DefaultExtraOptionKeyPattern.String()))
		g.Expect(DefaultExtraOptionKeyPattern.MatchString("with-dash")).To(gomega.BeFalse())
		g.Expect(DefaultExtraOptionKeyPattern.MatchString("s3_store_host")).To(gomega.BeTrue())
	})
}
