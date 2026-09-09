// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Release",type="string",JSONPath=".status.installedRelease"
// +kubebuilder:printcolumn:name="Endpoint",type="string",JSONPath=".status.endpoint"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Cinder is the Schema for the cinders API. It deploys the Cinder block-storage
// service: the API server under uWSGI, the scheduler, one cinder-volume process
// per attached volume backend, the backup service, and the database, cache,
// message-bus and Keystone integrations.
//
// Storage backends attach out-of-band rather than living in this spec, the same
// inverted attachment GlanceBackend uses: a CinderBackend CR describes one
// volume backend, a CinderBackupBackend CR the backup driver. Adding or
// removing storage therefore never edits this CR.
type Cinder struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CinderSpec   `json:"spec,omitempty"`
	Status CinderStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// CinderList contains a list of Cinder.
type CinderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Cinder `json:"items"`
}

// CinderSpec defines the desired state of Cinder.
//
// The targetClusterRef transition rules (evaluated only on UPDATE) freeze the
// ref: adding it, removing it, or renaming it after creation is rejected at the
// schema layer, so the guarantee holds even when the validating webhook is
// down. Moving a service between clusters is not a supported mutation.
//
// The volume and backup rules pin the two single-writer Deployments at one
// replica on the Recreate strategy. Both services own the storage they manage
// through a host identity rather than through a lock: a second process under
// the same identity, or a surge pod overlapping the outgoing one during a
// rolling update, has the same volume state open twice. The NFS drivers refuse
// that outright, so the rules are schema-level rather than webhook-level.
//
// The keystoneEndpoint/serviceUser pairing rule keeps a half-configured
// Keystone integration out of the API: the endpoint names who to authenticate
// against and the service user carries the credentials, and either alone
// renders a [keystone_authtoken] section the service cannot use. The keyManager
// rule follows from it, because castellan reaches Barbican with the same
// Keystone credentials.
//
// The gateway rule pairs external exposure with the Keystone integration.
// spec.keystoneEndpoint is optional so the storage path can be exercised on a
// cluster that runs no identity service, but a Cinder without it renders
// auth_strategy = noauth, a WSGI pipeline that takes the project from the
// request URL and validates no token at all. Publishing that through a Gateway
// hands every volume operation of every project to anyone who resolves the
// hostname, so the two are rejected together rather than deployed.
// +kubebuilder:validation:XValidation:rule="has(self.targetClusterRef) == has(oldSelf.targetClusterRef)",message="targetClusterRef is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.targetClusterRef) || !has(oldSelf.targetClusterRef) || self.targetClusterRef.name == oldSelf.targetClusterRef.name",message="targetClusterRef is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.volume) || !has(self.volume.deployment) || !has(self.volume.deployment.replicas) || self.volume.deployment.replicas == 1",message="volume/backup deployments run exactly one replica (the NFS drivers refuse active/active)"
// +kubebuilder:validation:XValidation:rule="!has(self.backup) || !has(self.backup.deployment) || !has(self.backup.deployment.replicas) || self.backup.deployment.replicas == 1",message="volume/backup deployments run exactly one replica (the NFS drivers refuse active/active)"
// +kubebuilder:validation:XValidation:rule="!has(self.volume) || !has(self.volume.deployment) || !has(self.volume.deployment.strategy) || !has(self.volume.deployment.strategy.type) || self.volume.deployment.strategy.type == 'Recreate'",message="volume/backup deployments must use the Recreate strategy"
// +kubebuilder:validation:XValidation:rule="!has(self.backup) || !has(self.backup.deployment) || !has(self.backup.deployment.strategy) || !has(self.backup.deployment.strategy.type) || self.backup.deployment.strategy.type == 'Recreate'",message="volume/backup deployments must use the Recreate strategy"
// +kubebuilder:validation:XValidation:rule="has(self.keystoneEndpoint) == has(self.serviceUser)",message="keystoneEndpoint and serviceUser must be set together"
// +kubebuilder:validation:XValidation:rule="!has(self.keyManager) || has(self.keystoneEndpoint)",message="keyManager requires keystoneEndpoint (castellan authenticates against Keystone)"
// +kubebuilder:validation:XValidation:rule="!has(self.gateway) || has(self.keystoneEndpoint)",message="gateway requires keystoneEndpoint: without it the API renders auth_strategy = noauth, so a Gateway would serve every volume operation unauthenticated"
type CinderSpec struct {
	// OpenStackRelease names the OpenStack release this operator deploys and
	// drives. It governs install/upgrade release tracking:
	// status.installedRelease is promoted to this value after a successful
	// db-sync. It is deliberately kept separate from the image tag so
	// digest-pinned images keep working: pinning spec.image.digest disables
	// tag-based release tracking, but this field still tells the operator which
	// schema to converge to.
	//
	// The pattern matches the OpenStack date-based release scheme (YYYY.N where N
	// is 1 or 2, the two-releases-per-year cadence, e.g. 2025.2, 2026.1). The
	// [12] minor class keeps this CRD pattern, the validating webhook, and
	// release.ParseRelease in agreement so a non-cadence minor (e.g. 2025.9) is
	// rejected at every layer.
	// +kubebuilder:validation:Pattern=`^\d{4}\.[12]$`
	OpenStackRelease string `json:"openStackRelease"`

	// Image defines the Cinder container image reference. Like the sibling
	// operators, the field carries no immutability rule: image upgrades are
	// routine. Every process this operator projects (API, scheduler, volume,
	// backup, and the db-sync and purge Jobs) runs this one image.
	Image commonv1.ImageSpec `json:"image"`

	// Database defines the MariaDB connection parameters.
	// Supports managed (clusterRef) and brownfield (host/port) modes. The
	// clusterRef/host mutual-exclusivity rule and the credentialsMode
	// (Static/Dynamic) contract are inherited from commonv1.DatabaseSpec, so they
	// hold here without per-field duplication.
	Database commonv1.DatabaseSpec `json:"database"`

	// Cache defines the Memcached cache configuration.
	// Supports managed (clusterRef) and brownfield (servers) modes; the
	// clusterRef/servers mutual-exclusivity rule lives on commonv1.CacheSpec.
	Cache commonv1.CacheSpec `json:"cache"`

	// Messaging defines the RabbitMQ connection Cinder publishes its RPC calls
	// and notifications on. It is required rather than optional: the API hands
	// every volume request to the scheduler over the bus, the scheduler hands it
	// to a cinder-volume, and backup jobs travel the same way, so a Cinder
	// without a broker accepts requests nothing acts on.
	Messaging commonv1.MessagingSpec `json:"messaging"`

	// API groups the knobs of the cinder-api Deployment: the pod-level
	// deployment block and the uWSGI application-server parameters. It is the
	// only one of the four process blocks that scales horizontally, so
	// spec.autoscaling targets this Deployment alone.
	// +optional
	API CinderAPISpec `json:"api,omitempty"`

	// Scheduler groups the knobs of the cinder-scheduler Deployment.
	// +optional
	Scheduler CinderSchedulerSpec `json:"scheduler,omitempty"`

	// Volume groups the knobs shared by every cinder-volume Deployment. The
	// operator projects one Deployment per attached CinderBackend and applies
	// this block to all of them.
	// +optional
	Volume CinderVolumeSpec `json:"volume,omitempty"`

	// Backup groups the knobs of the cinder-backup Deployment.
	// +optional
	Backup CinderBackupSpec `json:"backup,omitempty"`

	// KeystoneEndpoint is the Keystone endpoint URL Cinder authenticates
	// against. It renders as [keystone_authtoken] auth_url in cinder.conf.
	// Cinder connects to this URL server-side (token validation on every API
	// request), so it must be reachable from the Cinder pods. For a colocated
	// control plane that is the cluster-local Service URL, never an externally
	// routable address that only resolves outside the cluster. A plain URL field
	// keeps the operator decoupled from the keystone-operator; the c5c3
	// ControlPlane operator projects it from its Keystone child by naming
	// convention.
	//
	// It is optional, and a Cinder that omits it omits spec.serviceUser too (the
	// pairing rule on this spec): the service is then deployed without the
	// Keystone integration, which is how the operator's own suites exercise the
	// storage path on a cluster that runs no identity service.
	// +optional
	// +kubebuilder:validation:Pattern=`^https?://`
	KeystoneEndpoint string `json:"keystoneEndpoint,omitempty"`

	// KeystonePublicEndpoint is the browser/client-facing Keystone base URL
	// Cinder advertises as [keystone_authtoken] www_authenticate_uri, the
	// address a 401 response points unauthenticated clients at. Optional: when
	// empty the operator falls back to KeystoneEndpoint (see
	// EffectiveKeystonePublicEndpoint), which is correct only when the internal
	// and public Keystone URLs coincide.
	// +optional
	// +kubebuilder:validation:Pattern=`^https?://`
	KeystonePublicEndpoint string `json:"keystonePublicEndpoint,omitempty"`

	// ServiceUser identifies the Keystone service account Cinder authenticates
	// as and the Secret holding its password. It is required exactly when
	// spec.keystoneEndpoint is set (the pairing rule on this spec).
	// +optional
	ServiceUser *ServiceUserSpec `json:"serviceUser,omitempty"`

	// Region is the Keystone region Cinder authenticates against
	// ([keystone_authtoken] region_name). Optional: when empty the option is
	// omitted and Cinder uses the Keystone catalog's default region.
	// +optional
	Region string `json:"region,omitempty"`

	// GlanceEndpoint is the Glance API endpoint Cinder fetches image data from
	// ([DEFAULT] glance_api_servers), which is what serves a create-volume-from-
	// image request and a volume upload back to an image. Optional: when empty
	// the option is omitted and Cinder resolves Glance from the Keystone
	// catalog, which a Cinder without spec.keystoneEndpoint cannot do, so a
	// Keystone-free deployment that needs image-backed volumes sets this.
	// +optional
	// +kubebuilder:validation:Pattern=`^https?://`
	GlanceEndpoint string `json:"glanceEndpoint,omitempty"`

	// KeyManager selects the castellan key manager Cinder encrypts volumes
	// through, rendered as the [key_manager] backend plus the backend's own
	// section. When omitted, no key manager is configured and encrypted volume
	// types cannot be used. Setting it requires spec.keystoneEndpoint (the rule
	// on this spec).
	// +optional
	KeyManager *KeyManagerSpec `json:"keyManager,omitempty"`

	// InternalTenant names the Keystone project and user Cinder owns its
	// internal volumes as: the image-volume cache keeps its cached volumes
	// there, so they belong to the deployment rather than to the tenant whose
	// request happened to populate the cache. It is required before any
	// CinderBackend enables spec.imageVolumeCache.
	// +optional
	InternalTenant *InternalTenantSpec `json:"internalTenant,omitempty"`

	// DBPurge tunes the recurring database purge that hard-deletes the rows
	// Cinder only ever soft-deletes. A nil block resolves exactly like an empty
	// struct: the purge runs on every Cinder, since an unbounded soft-delete
	// backlog is a deferred outage rather than a posture worth offering.
	// +optional
	DBPurge *DBPurgeSpec `json:"dbPurge,omitempty"`

	// Gateway configures external exposure of the Cinder API via a Gateway API
	// HTTPRoute. When set, the operator creates an HTTPRoute targeting the {name}
	// Service and attaches it to the referenced pre-existing Gateway. When removed
	// (nil), the HTTPRoute is deleted. The Gateway and GatewayClass are
	// infrastructure concerns managed outside this operator.
	// +optional
	Gateway *GatewaySpec `json:"gateway,omitempty"`

	// NetworkPolicy configures network isolation for Cinder pods.
	// When set, a NetworkPolicy is created restricting ingress and egress traffic.
	// When removed (nil), the NetworkPolicy is deleted and traffic flows
	// unrestricted.
	// +optional
	NetworkPolicy *NetworkPolicySpec `json:"networkPolicy,omitempty"`

	// Autoscaling configures horizontal pod autoscaling for the Cinder API
	// deployment. When set, a HorizontalPodAutoscaler is created targeting the
	// deployment. When removed, the HPA is deleted. It reaches the API
	// Deployment only: the scheduler is not sized by request rate, and the
	// volume and backup Deployments are pinned at one replica.
	// +optional
	Autoscaling *AutoscalingSpec `json:"autoscaling,omitempty"`

	// Logging configures oslo.log output for the Cinder containers.
	// When unset, the defaulting webhook materializes a LoggingSpec with
	// Format=text, Level=INFO, Debug=false.
	// +optional
	Logging *LoggingSpec `json:"logging,omitempty"`

	// ExtraConfig provides free-form INI sections for configuration not covered by
	// explicit CRD fields. It is the deliberate escape hatch for options that have
	// no dedicated knob of their own.
	// +optional
	ExtraConfig map[string]map[string]string `json:"extraConfig,omitempty"`

	// SecretStoreRef selects the External Secrets store the operator routes this
	// Cinder's ExternalSecrets and PushSecrets through. When omitted the operator
	// uses the shared cluster-scoped openbao-cluster-store, so existing
	// deployments keep working unchanged. Set kind to SecretStore with the name of
	// a namespaced store in THIS Cinder's namespace to reach OpenBao as a
	// per-tenant identity. The ControlPlane operator projects this field onto the
	// Cinder it owns, so operators normally configure it there rather than here.
	// +optional
	SecretStoreRef *commonv1.SecretStoreRefSpec `json:"secretStoreRef,omitempty"`

	// PolicyOverrides defines custom oslo.policy rules for the service.
	// When set, the operator renders a policy.yaml and configures
	// oslo_policy.policy_file automatically.
	// +optional
	// +kubebuilder:validation:XValidation:rule="(has(self.rules) && size(self.rules) > 0) || self.configMapRef != null",message="at least one of rules or configMapRef must be set"
	// The empty rule-name and rule-value constraints are enforced by the
	// XValidation markers on commonv1.PolicySpec itself, so they apply to every
	// PolicySpec field across operators without per-field duplication.
	PolicyOverrides *commonv1.PolicySpec `json:"policyOverrides,omitempty"`

	// TargetClusterRef names the registered target cluster that receives this
	// Cinder's children: Deployments, ConfigMaps, Secrets, and the database
	// CRs. The CR itself stays on the management cluster, and so do its status
	// and its finalizers. When omitted, the children are created on the local
	// cluster (the management cluster the operator runs on) and the deployment
	// behaves exactly like a single-cluster one. The field is immutable (see the
	// transition rules on this spec).
	//
	// A child written to the target carries no owner reference, since nothing
	// on that cluster resolves one into the management cluster. Three labels
	// name the owner instead: openstack.c5c3.io/owner-kind,
	// openstack.c5c3.io/owner-name, and openstack.c5c3.io/owner-namespace.
	// Deleting the CR deletes the children that carry its labels, under a
	// finalizer the CR installs whenever the ref is set, so a target cluster
	// may have the service CRDs installed. A cluster deregistered past the
	// abandon window is the exception: its children cannot be reached and stay
	// behind on it.
	// +optional
	TargetClusterRef *commonv1.TargetClusterRefSpec `json:"targetClusterRef,omitempty"`
}

// DeploymentSpec, AutoscalingSpec, NetworkPolicySpec,
// NetworkPolicyIngressSource, LoggingSpec, GatewaySpec, GatewayParentRefSpec,
// and UWSGISpec are aliased to the shared commonv1 definitions — commonv1
// carries the canonical per-field godoc and validation markers. The aliases
// keep call sites (cinderv1alpha1.DeploymentSpec and bare DeploymentSpec{}
// literals alike) consistent with the sibling operators.
type (
	DeploymentSpec             = commonv1.DeploymentSpec
	AutoscalingSpec            = commonv1.AutoscalingSpec
	NetworkPolicySpec          = commonv1.NetworkPolicySpec
	NetworkPolicyIngressSource = commonv1.NetworkPolicyIngressSource
	LoggingSpec                = commonv1.LoggingSpec
	GatewaySpec                = commonv1.GatewaySpec
	GatewayParentRefSpec       = commonv1.GatewayParentRefSpec
	UWSGISpec                  = commonv1.UWSGISpec
)

// CinderAPISpec groups the knobs of the cinder-api Deployment.
type CinderAPISpec struct {
	// Deployment groups the pod-level knobs for the API Deployment (replicas,
	// resources, rollout strategy, graceful-termination timings, and scheduling
	// constraints).
	// +optional
	Deployment DeploymentSpec `json:"deployment,omitempty"`

	// UWSGI configures the uWSGI application server the API runs under. When nil
	// the operator uses the shared uWSGI defaults.
	// +optional
	UWSGI *UWSGISpec `json:"uwsgi,omitempty"`
}

// CinderSchedulerSpec groups the knobs of the cinder-scheduler Deployment. The
// scheduler picks a backend for every volume request the API accepts, off the
// message bus rather than off HTTP, which is why it gets its own Deployment
// instead of sharing the API's.
//
// Replicas resolve to one rather than the shared DeploymentSpec default of
// three, and may be raised: schedulers are peers that read the same backend
// capabilities off the bus and hold no state between requests. Every scheduler
// pod renders the same [DEFAULT] host, "<cinder>-scheduler", so the service
// registry carries one scheduler entry however many replicas run. The
// defaulting webhook applies that one (a later commit on this branch).
type CinderSchedulerSpec struct {
	// Deployment groups the pod-level knobs for the scheduler Deployment.
	// +optional
	Deployment DeploymentSpec `json:"deployment,omitempty"`
}

// CinderVolumeSpec groups the knobs shared by every cinder-volume Deployment.
// The operator projects one Deployment per attached CinderBackend and applies
// this block to all of them, so the knobs are uniform across backends rather
// than per-backend.
//
// Two of them are constrained by the drivers, through the CEL rules on
// CinderSpec: replicas must be one and the rollout strategy must be Recreate.
// A cinder-volume owns its backend through a host identity rather than through
// a lock, so a second process under the same identity (a second replica, or
// the surge pod of a rolling update overlapping the outgoing one) has the same
// volume state open twice, which the NFS drivers refuse. Note that the shared
// DeploymentSpec defaults replicas to three as soon as this block is present,
// so a CR that sets anything here spells out replicas: 1 alongside it.
type CinderVolumeSpec struct {
	// Deployment groups the pod-level knobs for every volume Deployment.
	// +optional
	Deployment DeploymentSpec `json:"deployment,omitempty"`
}

// CinderBackupSpec groups the knobs of the cinder-backup Deployment. It carries
// the same one-replica and Recreate constraints CinderVolumeSpec documents, for
// the same reason: the backup service owns its target through a host identity.
//
// The defaulting webhook raises the memory limit to 2Gi for this block (a later
// commit on this branch). A backup reads a volume in chunks and compresses each
// chunk in memory, so the shared 512Mi limit the other Deployments run under
// puts the process at risk of being killed mid-backup.
type CinderBackupSpec struct {
	// Deployment groups the pod-level knobs for the backup Deployment.
	// +optional
	Deployment DeploymentSpec `json:"deployment,omitempty"`
}

// ServiceUserSpec identifies the Keystone service account Cinder uses to validate
// tokens and call other services, and references the Secret holding its password.
// The name and domain fields are optional; the defaulting webhook materializes
// them (username cinder, projectName service, userDomainName and
// projectDomainName Default) in a later commit, so a minimal CR need only supply
// the password Secret reference.
type ServiceUserSpec struct {
	// Username is the Keystone username Cinder authenticates as
	// ([keystone_authtoken] username). Webhook-defaulted to "cinder".
	// +optional
	Username string `json:"username,omitempty"`

	// ProjectName is the Keystone project the service user scopes to
	// ([keystone_authtoken] project_name). Webhook-defaulted to "service".
	// +optional
	ProjectName string `json:"projectName,omitempty"`

	// UserDomainName is the domain the service user lives in
	// ([keystone_authtoken] user_domain_name). Webhook-defaulted to "Default".
	// +optional
	UserDomainName string `json:"userDomainName,omitempty"`

	// ProjectDomainName is the domain the service project lives in
	// ([keystone_authtoken] project_domain_name). Webhook-defaulted to "Default".
	// +optional
	ProjectDomainName string `json:"projectDomainName,omitempty"`

	// SecretRef references the Secret holding the service user's password. The
	// key is webhook-defaulted to "password" in a later commit.
	SecretRef commonv1.SecretRefSpec `json:"secretRef"`
}

// KeyManagerType enumerates the castellan key managers Cinder can encrypt
// volumes through. Barbican is the only one supported.
// +kubebuilder:validation:Enum=Barbican
type KeyManagerType string

const (
	// KeyManagerTypeBarbican selects the castellan Barbican key manager.
	KeyManagerTypeBarbican KeyManagerType = "Barbican"
)

// KeyManagerSpec selects the key manager castellan stores volume-encryption
// keys in. The union rule enforces "exactly one key-manager block matching
// spec.keyManager.type" at the schema layer, so it holds even when the
// validating webhook is down.
// +kubebuilder:validation:XValidation:rule="(self.type == 'Barbican') == has(self.barbican)",message="exactly one key-manager block matching spec.keyManager.type must be set (type Barbican requires spec.keyManager.barbican)"
type KeyManagerSpec struct {
	// Type selects the castellan key manager. Barbican is the only supported
	// value.
	Type KeyManagerType `json:"type"`

	// Barbican configures the Barbican key manager. Required exactly when type
	// is Barbican (union rule above).
	// +optional
	Barbican *BarbicanKeyManagerSpec `json:"barbican,omitempty"`
}

// BarbicanKeyManagerSpec configures the castellan Barbican key manager. It
// carries the endpoint alone: castellan reaches Barbican with the Keystone
// credentials from spec.serviceUser, so there is no second credential to
// configure here.
type BarbicanKeyManagerSpec struct {
	// Endpoint is the Barbican API endpoint castellan calls
	// ([barbican] barbican_endpoint). Cinder connects to it server-side, so it
	// must be reachable from the Cinder pods.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^https?://`
	Endpoint string `json:"endpoint"`
}

// InternalTenantSpec names the Keystone project and user Cinder owns its
// internal volumes as ([DEFAULT] cinder_internal_tenant_project_id and
// cinder_internal_tenant_user_id). The image-volume cache is what needs them:
// a cached volume is created once and reused across projects, so it belongs to
// the deployment rather than to whichever tenant's request populated it.
//
// Both fields are IDs, not names. Cinder passes them to the volume API without
// resolving them through Keystone, so a name reaches the backend as a project
// that does not exist and the cached volume is created under nothing.
type InternalTenantSpec struct {
	// ProjectID is the Keystone project ID internal volumes are created in.
	// +kubebuilder:validation:MinLength=1
	ProjectID string `json:"projectID"`

	// UserID is the Keystone user ID internal volumes are created as.
	// +kubebuilder:validation:MinLength=1
	UserID string `json:"userID"`
}

// DBPurgeSpec tunes the recurring database purge. Cinder never hard-deletes on
// its own: deleting a volume, a snapshot or a backup only flips its row to
// deleted, so the tables grow for the lifetime of the deployment. The operator
// projects a CronJob running "cinder-manage db purge <days>", which hard-deletes
// every soft-deleted row older than the retention window.
//
// The operator resolves the knobs at reconcile time rather than materializing
// them into the CR, so a field left unset keeps tracking the operator defaults
// across upgrades instead of freezing today's values into the stored CR. A nil
// block therefore resolves exactly like an empty struct: 30 days of retention,
// swept daily at 00:01.
type DBPurgeSpec struct {
	// RetentionDays is how long a soft-deleted row survives before the purge
	// hard-deletes it — the age argument "cinder-manage db purge" takes. When
	// unset the operator resolves 30 days; the lower bound of one day keeps the
	// purge from racing the rows an in-flight request just wrote. Lowering it
	// applies retroactively at the next firing, so the validating webhook warns
	// on a reduction.
	// +optional
	// +kubebuilder:validation:Minimum=1
	RetentionDays *int32 `json:"retentionDays,omitempty"`

	// Schedule is the standard cron expression the purge CronJob runs on. When
	// empty the operator resolves "1 0 * * *". The value is checked by the
	// validating webhook rather than by a CRD pattern — as with the keystone
	// schedules, the accepted grammar includes descriptors such as @daily, which
	// no regex expresses without also rejecting valid expressions.
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// Suspend pauses the purge CronJob without deleting it. It is the escape
	// hatch for a brownfield deployment onboarding onto this operator: the first
	// run applies the retention window retroactively to a backlog that has never
	// been purged, so an operator who wants to stage that can suspend the
	// CronJob, raise RetentionDays to cover the deployment's full history, and
	// step it down. The condition stays True while suspended — a paused purge is
	// a deliberate posture, not a failure.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// CinderStatus defines the observed state of Cinder.
type CinderStatus struct {
	// Conditions represent the latest available observations of the Cinder state.
	// Each condition carries an ObservedGeneration so consumers can tell a stale
	// condition from one reflecting the current spec; use the conditions helper
	// (internal/common/conditions) to upsert them.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ObservedGeneration is the .metadata.generation the controller last
	// reconciled, so a stale status is distinguishable from a current one.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Endpoint is the Cinder API endpoint URL clients use.
	Endpoint string `json:"endpoint,omitempty"`

	// InstalledRelease is the OpenStack release whose database schema is currently
	// installed, promoted to spec.openStackRelease after a successful db-sync.
	InstalledRelease string `json:"installedRelease,omitempty"`

	// TargetRelease is the spec.openStackRelease being converged to during an
	// active upgrade.
	TargetRelease string `json:"targetRelease,omitempty"`

	// UpgradePhase is the current expand-migrate-contract phase during an active
	// release upgrade (Expanding, Migrating, RollingUpdate, Contracting); empty
	// when no upgrade is in flight.
	UpgradePhase commonv1.UpgradePhase `json:"upgradePhase,omitempty"`

	// VolumeServices lists the host identity the operator rendered for each
	// attached CinderBackend, sorted by backend name. It reports what the
	// operator configured, not what Cinder reports back: an entry appears as
	// soon as the backend's Deployment is projected, and says nothing about
	// whether that cinder-volume registered with the scheduler. Read the
	// backend's own conditions for that.
	// +optional
	VolumeServices []VolumeServiceStatus `json:"volumeServices,omitempty"`
}

// VolumeServiceStatus pairs an attached CinderBackend with the host identity
// its cinder-volume process runs under.
type VolumeServiceStatus struct {
	// Backend is the name of the CinderBackend CR this entry describes.
	Backend string `json:"backend"`

	// Host is the identity the backend's cinder-volume registers under,
	// "<cinder>@<backend>". It is the value the operator rendered, which is what
	// makes it stable across pod replacements: the volumes a backend owns are
	// keyed by this string, so it must survive a Deployment roll.
	Host string `json:"host"`
}

// EffectiveKeystonePublicEndpoint resolves the [keystone_authtoken]
// www_authenticate_uri value: the explicit KeystonePublicEndpoint when set,
// otherwise KeystoneEndpoint. It is resolved at render time rather than
// webhook-defaulted so a later edit to keystoneEndpoint keeps being tracked by
// the fallback instead of freezing a once-defaulted value.
func (s *CinderSpec) EffectiveKeystonePublicEndpoint() string {
	if s.KeystonePublicEndpoint != "" {
		return s.KeystonePublicEndpoint
	}
	return s.KeystoneEndpoint
}

func init() {
	SchemeBuilder.Register(&Cinder{}, &CinderList{})
}
