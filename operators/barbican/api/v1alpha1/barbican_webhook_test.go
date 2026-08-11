// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"strings"
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// validBarbican returns a minimal Barbican CR that passes every validation rule.
// Tests mutate single fields to exercise individual rules.
func validBarbican() *Barbican {
	return &Barbican{
		ObjectMeta: metav1.ObjectMeta{Name: "test-barbican", Namespace: "openstack"},
		Spec: BarbicanSpec{
			OpenStackRelease: "2025.2",
			Deployment:       DeploymentSpec{Replicas: 3},
			Image: commonv1.ImageSpec{
				Repository: "ghcr.io/c5c3/barbican",
				Tag:        "2025.2",
			},
			Database: commonv1.DatabaseSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "mariadb"},
			},
			Cache: commonv1.CacheSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "memcached"},
				Backend:    commonv1.DefaultCacheBackend,
			},
			KeystoneEndpoint: "http://keystone.openstack.svc.cluster.local:5000/v3",
			ServiceUser: ServiceUserSpec{
				SecretRef: commonv1.SecretRefSpec{Name: "barbican-service-password", Key: "password"},
			},
		},
	}
}

func TestBarbicanDefault_MaterializesServiceUserAndLoggingDefaults(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &BarbicanWebhook{}

	// Start from a CR whose service-user identity fields and secretRef key are
	// empty so the defaulter has to fill all five.
	obj := validBarbican()
	obj.Spec.ServiceUser = ServiceUserSpec{SecretRef: commonv1.SecretRefSpec{Name: "barbican-service-password"}}
	obj.Spec.Cache.Backend = ""

	g.Expect(w.Default(context.Background(), obj)).To(gomega.Succeed())

	g.Expect(obj.Spec.ServiceUser.Username).To(gomega.Equal("barbican"))
	g.Expect(obj.Spec.ServiceUser.ProjectName).To(gomega.Equal("service"))
	g.Expect(obj.Spec.ServiceUser.UserDomainName).To(gomega.Equal("Default"))
	g.Expect(obj.Spec.ServiceUser.ProjectDomainName).To(gomega.Equal("Default"))
	g.Expect(obj.Spec.ServiceUser.SecretRef.Key).To(gomega.Equal("password"))

	// The shared block defaults come along too.
	g.Expect(obj.Spec.Deployment.Resources).NotTo(gomega.BeNil())
	g.Expect(obj.Spec.Cache.Backend).To(gomega.Equal(commonv1.DefaultCacheBackend))
	g.Expect(obj.Spec.Logging).NotTo(gomega.BeNil())
	g.Expect(obj.Spec.Logging.Format).To(gomega.Equal("text"))
	g.Expect(obj.Spec.Logging.Level).To(gomega.Equal("INFO"))
}

func TestBarbicanDefault_PreservesExplicitServiceUserValues(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &BarbicanWebhook{}

	obj := validBarbican()
	obj.Spec.ServiceUser = ServiceUserSpec{
		Username:          "kms-svc",
		ProjectName:       "keymanager",
		UserDomainName:    "Corp",
		ProjectDomainName: "Corp",
		SecretRef:         commonv1.SecretRefSpec{Name: "custom-secret", Key: "custom-key"},
	}

	g.Expect(w.Default(context.Background(), obj)).To(gomega.Succeed())

	g.Expect(obj.Spec.ServiceUser.Username).To(gomega.Equal("kms-svc"))
	g.Expect(obj.Spec.ServiceUser.ProjectName).To(gomega.Equal("keymanager"))
	g.Expect(obj.Spec.ServiceUser.UserDomainName).To(gomega.Equal("Corp"))
	g.Expect(obj.Spec.ServiceUser.ProjectDomainName).To(gomega.Equal("Corp"))
	g.Expect(obj.Spec.ServiceUser.SecretRef.Key).To(gomega.Equal("custom-key"))
}

// Barbican ships a WSGI application only, so spec.apiServer carries the uWSGI
// block alone: a nil block stays nil (the reconciler uses its hardcoded
// defaults), a present-but-zero block is filled from the shared constants, and an
// explicit value survives.
func TestBarbicanDefault_UWSGISemantics(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &BarbicanWebhook{}

	nilBlock := validBarbican()
	g.Expect(w.Default(context.Background(), nilBlock)).To(gomega.Succeed())
	g.Expect(nilBlock.Spec.APIServer).To(gomega.BeNil())

	zero := validBarbican()
	zero.Spec.APIServer = &APIServerSpec{UWSGI: &UWSGISpec{}}
	g.Expect(w.Default(context.Background(), zero)).To(gomega.Succeed())
	g.Expect(zero.Spec.APIServer.UWSGI.Processes).To(gomega.Equal(commonv1.DefaultUWSGIProcesses))
	g.Expect(zero.Spec.APIServer.UWSGI.Threads).To(gomega.Equal(commonv1.DefaultUWSGIThreads))
	g.Expect(zero.Spec.APIServer.UWSGI.HTTPKeepAlive).To(gomega.HaveValue(gomega.BeTrue()))

	explicit := validBarbican()
	explicit.Spec.APIServer = &APIServerSpec{UWSGI: &UWSGISpec{
		Processes:     4,
		Threads:       2,
		HTTPKeepAlive: ptr.To(false),
	}}
	g.Expect(w.Default(context.Background(), explicit)).To(gomega.Succeed())
	g.Expect(explicit.Spec.APIServer.UWSGI.Processes).To(gomega.Equal(int32(4)))
	g.Expect(explicit.Spec.APIServer.UWSGI.Threads).To(gomega.Equal(int32(2)))
	g.Expect(explicit.Spec.APIServer.UWSGI.HTTPKeepAlive).To(gomega.HaveValue(gomega.BeFalse()))
}

// The db-clean knobs are resolved at reconcile time, never stamped into the
// stored CR: an unset block must survive admission unset so it keeps tracking the
// operator defaults across upgrades.
func TestBarbicanDefault_LeavesDBCleanUnset(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &BarbicanWebhook{}

	obj := validBarbican()
	g.Expect(w.Default(context.Background(), obj)).To(gomega.Succeed())
	g.Expect(obj.Spec.DBClean).To(gomega.BeNil())
}

func TestBarbicanValidateCreate_ValidSpecAccepted(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &BarbicanWebhook{}

	_, err := w.ValidateCreate(context.Background(), validBarbican())
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

func TestBarbicanValidateCreate_RejectionTable(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(o *Barbican)
		wantSub string
	}{
		{
			name:    "zero replicas rejected",
			mutate:  func(o *Barbican) { o.Spec.Deployment.Replicas = 0 },
			wantSub: "replicas must be at least 1",
		},
		{
			name: "image tag and digest both set rejected",
			mutate: func(o *Barbican) {
				o.Spec.Image.Digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			},
			wantSub: "exactly one of image.tag or image.digest",
		},
		{
			name:    "database clusterRef and host both set rejected",
			mutate:  func(o *Barbican) { o.Spec.Database.Host = "mariadb.example.com" },
			wantSub: "exactly one of clusterRef or host",
		},
		{
			name: "dynamic credentials without clusterRef rejected",
			mutate: func(o *Barbican) {
				o.Spec.Database.ClusterRef = nil
				o.Spec.Database.Host = "mariadb.example.com"
				o.Spec.Database.CredentialsMode = commonv1.CredentialsModeDynamic
			},
			wantSub: "credentialsMode Dynamic requires clusterRef",
		},
		{
			name:    "cache clusterRef and servers both set rejected",
			mutate:  func(o *Barbican) { o.Spec.Cache.Servers = []string{"memcached-0:11211"} },
			wantSub: "exactly one of clusterRef or servers",
		},
		// Both cache shapes land in [keystone_authtoken].memcached_servers via
		// cache.ResolveServers, which the INI renderer writes verbatim. A newline
		// appends a second auth_url to that section and oslo.config keeps the last
		// value for a non-multi option, so keystonemiddleware would validate every
		// incoming token against an attacker-controlled Keystone.
		{
			name: "cache server with a newline rejected",
			mutate: func(o *Barbican) {
				o.Spec.Cache.ClusterRef = nil
				o.Spec.Cache.Servers = []string{"memcached-0:11211\nauth_url = http://attacker.example/v3"}
			},
			wantSub: "must not contain a newline or carriage return",
		},
		{
			name: "serviceUser username with a newline rejected",
			mutate: func(o *Barbican) {
				o.Spec.ServiceUser.Username = "barbican\npassword = hunter2"
			},
			wantSub: "must not contain a newline or carriage return",
		},
		// gateway.hostname is rendered verbatim as [DEFAULT] host_href. [DEFAULT]
		// is the first section in the document, so an injected line lands in a
		// section the operator never writes and nothing overrides — including the
		// options extraConfig is explicitly blocked from reaching. The HTTPRoute
		// step would reject the hostname, but the config Secret is written and
		// mounted before it runs.
		{
			name: "gateway hostname with a newline rejected",
			mutate: func(o *Barbican) {
				o.Spec.Gateway = &GatewaySpec{
					Hostname:  "barbican.example.com\ndebug = True",
					ParentRef: GatewayParentRefSpec{Name: "gateway"},
				}
			},
			wantSub: "must not contain a newline or carriage return",
		},
		{
			name: "invalid secretStoreRef kind rejected",
			mutate: func(o *Barbican) {
				o.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{Name: "openbao", Kind: "Vault"}
			},
			wantSub: "spec.secretStoreRef.kind",
		},
		{
			name:    "empty keystoneEndpoint rejected",
			mutate:  func(o *Barbican) { o.Spec.KeystoneEndpoint = "" },
			wantSub: "keystoneEndpoint must be set",
		},
		{
			name:    "keystoneEndpoint without a host rejected",
			mutate:  func(o *Barbican) { o.Spec.KeystoneEndpoint = "http:///v3" },
			wantSub: "URL must include a host",
		},
		{
			name:    "unknown logging level rejected",
			mutate:  func(o *Barbican) { o.Spec.Logging = &LoggingSpec{Level: "TRACE"} },
			wantSub: "spec.logging.level",
		},
		{
			name: "topologySpreadConstraints with a foreign selector rejected",
			mutate: func(o *Barbican) {
				o.Spec.Deployment.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
					MaxSkew:           1,
					TopologyKey:       "kubernetes.io/hostname",
					WhenUnsatisfiable: corev1.DoNotSchedule,
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "barbican"},
					},
				}}
			},
			wantSub: "labelSelector.matchLabels must equal the Deployment selector labels",
		},
		{
			name: "empty policy rule value rejected",
			mutate: func(o *Barbican) {
				o.Spec.PolicyOverrides = &commonv1.PolicySpec{Rules: map[string]string{"secrets:get": ""}}
			},
			wantSub: "policy rule value must not be empty",
		},
		{
			name: "dbClean retentionDays below one rejected",
			mutate: func(o *Barbican) {
				o.Spec.DBClean = &DBCleanSpec{RetentionDays: ptr.To(int32(0))}
			},
			wantSub: "retentionDays must be at least 1",
		},
		{
			name: "rejected owned key in extraConfig",
			mutate: func(o *Barbican) {
				o.Spec.ExtraConfig = map[string]map[string]string{
					"vault_plugin": {"root_token_id": "s.root"},
				}
			},
			wantSub: "root_token_id is managed via",
		},
		{
			name: "extraConfig value with a newline rejected",
			mutate: func(o *Barbican) {
				o.Spec.ExtraConfig = map[string]map[string]string{
					"DEFAULT": {"debug": "true\n[vault_plugin]\nroot_token_id = s.root"},
				}
			},
			wantSub: "extraConfig key and value must not contain a newline or carriage return",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &BarbicanWebhook{}

			obj := validBarbican()
			tc.mutate(obj)
			_, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.wantSub))
		})
	}
}

// The cron grammar is the one dbClean rule with no schema counterpart: the field
// accepts descriptors such as @daily, which no CRD pattern expresses without also
// rejecting valid expressions. The parse error travels into the message so the
// author sees which field of the expression is wrong.
func TestBarbicanValidate_DBCleanScheduleGrammar(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &BarbicanWebhook{}

	bad := validBarbican()
	bad.Spec.DBClean = &DBCleanSpec{Schedule: "not a cron"}
	_, err := w.ValidateCreate(context.Background(), bad)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("invalid cron expression"))
	g.Expect(err.Error()).To(gomega.ContainSubstring("expected exactly 5 fields"))

	// A descriptor and a five-field expression are both accepted, and so is an
	// empty schedule (the operator resolves its default at reconcile time).
	for _, schedule := range []string{"", "@daily", "1 0 * * *"} {
		ok := validBarbican()
		ok.Spec.DBClean = &DBCleanSpec{Schedule: schedule}
		_, err := w.ValidateCreate(context.Background(), ok)
		g.Expect(err).NotTo(gomega.HaveOccurred(), "schedule %q must be accepted", schedule)
	}
}

// metadata.name is bounded by the child object with the tightest name budget, the
// "{name}-db-clean" CronJob. Nothing else in the CRD or the webhook bounds it, so
// without this rule a name the API server would refuse as a CronJob admits
// cleanly.
func TestBarbicanValidateCreate_NameLengthBoundedByDBCleanCronJob(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &BarbicanWebhook{}

	g.Expect(MaxBarbicanNameLength).To(gomega.Equal(43))

	atLimit := validBarbican()
	atLimit.Name = strings.Repeat("b", MaxBarbicanNameLength)
	_, err := w.ValidateCreate(context.Background(), atLimit)
	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"a name that still fits the 52-character CronJob budget must be accepted")

	tooLong := validBarbican()
	tooLong.Name = strings.Repeat("b", MaxBarbicanNameLength+1)
	_, err = w.ValidateCreate(context.Background(), tooLong)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("metadata.name")))
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("db-clean")))
}

// The bound is create-only. metadata.name is immutable, so on update it could
// only ever fire against a CR a pre-upgrade operator already admitted — and the
// validating webhook registers the update verb, so it also sees the
// finalizer-removal update reconcileDelete issues. Rejecting that would wedge the
// grandfathered CR in Terminating forever, with no field left to edit to repair
// it.
func TestBarbicanValidateUpdate_OverlongNameStaysUpdatable(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &BarbicanWebhook{}

	grandfathered := validBarbican()
	grandfathered.Name = strings.Repeat("b", MaxBarbicanNameLength+1)
	grandfathered.Finalizers = []string{"barbican.openstack.c5c3.io/finalizer"}

	deleting := grandfathered.DeepCopy()
	deleting.Finalizers = nil

	_, err := w.ValidateUpdate(context.Background(), grandfathered, deleting)
	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"an over-long grandfathered CR must stay updatable, or its deletion never completes")
}

// Adopting a brownfield database is the larger of the two irreversible
// clean-up events: the schedule is enabled by default, and its first firing
// applies the retention window retroactively to a backlog that has never been
// cleaned. The field doc says so, but the default is the destructive one, so
// admission echoes the consequence back rather than relying on the operator
// having read it.
func TestBarbicanValidateCreate_WarnsOnBrownfieldDBCleanFirstRun(t *testing.T) {
	brownfield := func() *Barbican {
		o := validBarbican()
		o.Spec.Database = commonv1.DatabaseSpec{
			Host: "mariadb.example.com", Port: 3306, Database: "barbican",
			SecretRef: commonv1.SecretRefSpec{Name: "barbican-db"},
		}
		return o
	}
	tests := []struct {
		name     string
		obj      func() *Barbican
		wantWarn bool
	}{
		{name: "brownfield with the clean-up running warns", obj: brownfield, wantWarn: true},
		{
			name: "brownfield with the clean-up suspended is silent",
			obj: func() *Barbican {
				o := brownfield()
				o.Spec.DBClean = &DBCleanSpec{Suspend: true}
				return o
			},
			wantWarn: false,
		},
		{
			// The operator provisions the schema itself, so the first run has no
			// history to purge.
			name:     "managed database is silent",
			obj:      validBarbican,
			wantWarn: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &BarbicanWebhook{}

			warnings, err := w.ValidateCreate(context.Background(), tc.obj())
			g.Expect(err).NotTo(gomega.HaveOccurred())

			matched := false
			for _, warning := range warnings {
				if strings.Contains(warning, "hard-deletes every row soft-deleted more than 90 days ago") {
					matched = true
				}
			}
			g.Expect(matched).To(gomega.Equal(tc.wantWarn))
		})
	}
}

// Shortening the retention window is the one edit on this CR that destroys data
// at the next firing with no undo, and a typo is indistinguishable from an
// intended change at admission time. The warning is what echoes it back.
func TestBarbicanValidateUpdate_WarnsOnReducedDBCleanRetention(t *testing.T) {
	newObjWith := func(c *DBCleanSpec) *Barbican {
		o := validBarbican()
		o.Spec.DBClean = c
		return o
	}
	tests := []struct {
		name     string
		old, new *DBCleanSpec
		wantWarn bool
	}{
		{
			name:     "explicit reduction warns",
			old:      &DBCleanSpec{RetentionDays: ptr.To(int32(90))},
			new:      &DBCleanSpec{RetentionDays: ptr.To(int32(9))},
			wantWarn: true,
		},
		{
			// An unset field resolves to the operator default, so setting a value
			// below it is just as much a reduction as lowering an explicit one.
			name:     "reduction below the resolved default warns",
			old:      nil,
			new:      &DBCleanSpec{RetentionDays: ptr.To(int32(7))},
			wantWarn: true,
		},
		{
			name:     "raising the retention is silent",
			old:      &DBCleanSpec{RetentionDays: ptr.To(int32(7))},
			new:      &DBCleanSpec{RetentionDays: ptr.To(int32(180))},
			wantWarn: false,
		},
		{
			name:     "an unrelated edit is silent",
			old:      &DBCleanSpec{RetentionDays: ptr.To(int32(7))},
			new:      &DBCleanSpec{RetentionDays: ptr.To(int32(7)), Schedule: "@weekly"},
			wantWarn: false,
		},
		{
			// Dropping the block restores the default (90), which is a widening from
			// 7 — not a reduction to zero.
			name:     "dropping the block below the default is silent",
			old:      &DBCleanSpec{RetentionDays: ptr.To(int32(7))},
			new:      nil,
			wantWarn: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &BarbicanWebhook{}

			warnings, err := w.ValidateUpdate(context.Background(), newObjWith(tc.old), newObjWith(tc.new))
			g.Expect(err).NotTo(gomega.HaveOccurred())
			if tc.wantWarn {
				g.Expect(warnings).To(gomega.ContainElement(
					gomega.ContainSubstring("spec.dbClean.retentionDays reduced"),
				))
			} else {
				g.Expect(warnings).NotTo(gomega.ContainElement(
					gomega.ContainSubstring("spec.dbClean.retentionDays reduced"),
				))
			}
		})
	}
}

func TestBarbicanValidate_ExtraConfigUnknownOptionRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &BarbicanWebhook{}

	obj := validBarbican()
	obj.Spec.ExtraConfig = map[string]map[string]string{
		"queue": {"not_an_option": "x"},
	}
	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("no such option in the barbican 2025.2 option catalog"))
}

// [kmip_plugin] is absent from both catalogs because the shipped image carries no
// pykmip, so the whole section is reported rather than the individual key.
func TestBarbicanValidate_ExtraConfigUnknownSectionRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &BarbicanWebhook{}

	obj := validBarbican()
	obj.Spec.ExtraConfig = map[string]map[string]string{
		"kmip_plugin": {"host": "kmip.example.com"},
	}
	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("no such section in the barbican 2025.2 option catalog"))
}

// A per-store section is named after a BarbicanSecretStore CR, so no release
// catalog can list it. The prefix expansion is what carries it past the scan,
// including options no catalog knows at all.
func TestBarbicanValidate_ExtraConfigPerStoreSectionExempt(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &BarbicanWebhook{}

	obj := validBarbican()
	obj.Spec.ExtraConfig = map[string]map[string]string{
		"secretstore:primary": {"some_store_local_option": "x"},
	}
	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// A release the build ships no catalog for must not block admission: the check
// fails open with exactly one warning, and the two misses are distinguishable.
func TestBarbicanValidate_ExtraConfigFailsOpenWithoutCatalog(t *testing.T) {
	tests := []struct {
		name    string
		release string
		wantSub string
	}{
		{
			name:    "unparseable release",
			release: "latest",
			wantSub: "spec.openStackRelease does not name an OpenStack release",
		},
		{
			name:    "release with no embedded catalog",
			release: "2024.2",
			wantSub: `no catalog for release "2024.2" is embedded in this operator build`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &BarbicanWebhook{}

			obj := validBarbican()
			obj.Spec.OpenStackRelease = tc.release
			// An option no catalog carries: with a catalog resolved this would be a
			// rejection, so the acceptance below is attributable to the fail-open path.
			obj.Spec.ExtraConfig = map[string]map[string]string{
				"queue": {"not_an_option": "x"},
			}

			warnings, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(warnings).To(gomega.ConsistOf(gomega.ContainSubstring(tc.wantSub)))
		})
	}
}

// A deprecated-but-accepted option is honored and reported, never rejected.
func TestBarbicanValidate_ExtraConfigDeprecatedOptionWarns(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &BarbicanWebhook{}

	obj := validBarbican()
	obj.Spec.ExtraConfig = map[string]map[string]string{
		"DEFAULT": {"logfile": "/var/log/barbican.log"},
	}
	warnings, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(warnings).To(gomega.ConsistOf(
		gomega.ContainSubstring("deprecated option in barbican 2025.2, replaced by [DEFAULT] log_file"),
	))
}

// The catalog check is re-run on update only when one of its inputs changed, so a
// CR whose extraConfig went stale-invalid against a regenerated catalog stays
// editable through every other field.
func TestBarbicanValidateUpdate_ExtraConfigCatalogGate(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &BarbicanWebhook{}

	stale := validBarbican()
	stale.Spec.ExtraConfig = map[string]map[string]string{
		"queue": {"not_an_option": "x"},
	}

	// Editing an unrelated field leaves the catalog check unrun.
	scaled := stale.DeepCopy()
	scaled.Spec.Deployment.Replicas = 5
	_, err := w.ValidateUpdate(context.Background(), stale, scaled)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// Touching extraConfig itself runs it again.
	edited := stale.DeepCopy()
	edited.Spec.ExtraConfig = map[string]map[string]string{
		"queue": {"still_not_an_option": "x"},
	}
	_, err = w.ValidateUpdate(context.Background(), stale, edited)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("no such option in the barbican 2025.2 option catalog"))
}

// --- spec.targetClusterRef (multicluster routing) ---

// TestBarbicanValidateUpdate_TargetClusterRefAddedRejected covers the presence
// flip upwards: the children of a CR created without a target cluster live on
// the management cluster, so naming one afterwards is rejected.
func TestBarbicanValidateUpdate_TargetClusterRefAddedRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &BarbicanWebhook{}
	old := validBarbican()
	newObj := validBarbican()
	newObj.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-1"}

	_, err := w.ValidateUpdate(context.Background(), old, newObj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("targetClusterRef is immutable"))
}

// TestBarbicanValidateUpdate_TargetClusterRefRemovedRejected covers the presence
// flip downwards: dropping the ref would strand the children on the cluster it
// named.
func TestBarbicanValidateUpdate_TargetClusterRefRemovedRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &BarbicanWebhook{}
	old := validBarbican()
	old.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-1"}
	newObj := validBarbican()

	_, err := w.ValidateUpdate(context.Background(), old, newObj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("targetClusterRef is immutable"))
}

// TestBarbicanValidateUpdate_TargetClusterRefChangedRejected covers a rename,
// which would re-point the reconciler at a cluster that holds none of the
// children.
func TestBarbicanValidateUpdate_TargetClusterRefChangedRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &BarbicanWebhook{}
	old := validBarbican()
	old.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-1"}
	newObj := validBarbican()
	newObj.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-2"}

	_, err := w.ValidateUpdate(context.Background(), old, newObj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("targetClusterRef is immutable"))
}

// TestBarbicanValidateUpdate_TargetClusterRefUnchangedAccepted proves the check
// freezes only the ref: an unrelated edit on a CR that names a target cluster
// still passes.
func TestBarbicanValidateUpdate_TargetClusterRefUnchangedAccepted(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &BarbicanWebhook{}
	old := validBarbican()
	old.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-1"}
	newObj := old.DeepCopy()
	newObj.Spec.Deployment.Replicas = 2

	_, err := w.ValidateUpdate(context.Background(), old, newObj)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// TestBarbicanValidateCreate_EmptyTargetClusterRefNameRejected is the
// defense-in-depth twin of the MinLength marker: a present ref must name a
// cluster.
func TestBarbicanValidateCreate_EmptyTargetClusterRefNameRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &BarbicanWebhook{}
	obj := validBarbican()
	obj.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: ""}

	_, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("targetClusterRef.name"))
	g.Expect(err.Error()).To(gomega.ContainSubstring("target cluster name must be set"))
}
