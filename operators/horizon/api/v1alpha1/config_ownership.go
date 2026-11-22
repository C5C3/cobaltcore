// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import "github.com/c5c3/cobaltcore/internal/common/config"

// The owned-config-key registry below records the flat Django settings the
// operator computes and renders into local_settings.py, so the post-merge
// health guard and the validating webhook can reason about user overrides from
// one source of truth. Every entry is flat (Section ""): Horizon renders a flat
// Django settings module, not an INI file.
//
// Three rules govern what belongs here:
//
//   - The registry is static. Conditionally rendered keys — the websso and
//     multiDomain settings, and SESSION_CACHE_ALIAS of cache sessions — are
//     registered unconditionally, because the registry documents "this key is
//     not the user's to set", not "this key is currently rendered". A key the
//     operator owns whenever it renders it stays owned even in the CRs where
//     that render is skipped.
//
//   - The session keys SESSION_ENGINE and SESSION_CACHE_ALIAS are reported,
//     not rejected, where the webhook admits them: SESSION_ENGINE on a CR
//     without spec.session, SESSION_CACHE_ALIAS on a CR without the block or
//     with backend SignedCookies. ValidateSession rejects SESSION_ENGINE beside
//     either backend and SESSION_CACHE_ALIAS beside backend Cache.
//
//   - The sanctioned-override exceptions OPENSTACK_ENDPOINT_TYPE and
//     SECURE_PROXY_SSL_HEADER are deliberately NOT registered. The code comments
//     in internal/controller/reconcile_config.go (the OPENSTACK_ENDPOINT_TYPE
//     block and the SECURE_PROXY_SSL_HEADER block) name an extraConfig override
//     of each as the supported escape hatch — a deployment fronting a remote
//     cloud overrides OPENSTACK_ENDPOINT_TYPE, and the proxy-ssl header stays
//     overridable per the ingress path — so a user override of either must stay
//     silent rather than be reported or rejected.

// sessionEngineImpact and sessionCacheAliasImpact are the Impacts of the two
// reported session keys. Each names what its extraConfig override does
// instead of the typed spec.session block.
const (
	sessionEngineImpact = "the typed session field selects the session store; a cache engine set " +
		"here keeps the sessions in the default cache alias, whose image-scoped KEY_PREFIX logs every " +
		"user out on each image change"
	sessionCacheAliasImpact = "the typed session field selects the session store; this setting takes " +
		"effect only beside a cache SESSION_ENGINE set in extraConfig, and backend Cache of the typed " +
		"field renders its own name-scoped \"sessions\" alias"
)

// OwnedConfigKeys is the registry of local_settings.py Django settings the
// operator owns. Reported entries are honored-but-surfaced when overridden in
// spec.extraConfig; the Rejected entries — SECRET_KEY and the settings backing
// the typed spec.websso and spec.multiDomain blocks — are blocked at admission
// by the validating webhook. The Rejected websso/multiDomain entries are built
// from the exported WebSSOSettingNames / MultiDomainSettingNames lists, so the
// setting names stay a single source of truth shared with the renderer and the
// webhook rather than a second copy that could drift.
var OwnedConfigKeys = buildOwnedConfigKeys()

func buildOwnedConfigKeys() []config.OwnedKey {
	keys := []config.OwnedKey{
		// Reported: emitted by defaultSettings (reconcile_config.go).
		{Key: "OPENSTACK_KEYSTONE_URL", OwnedBy: "operator-computed"},
		{Key: "ALLOWED_HOSTS", OwnedBy: "operator-computed"},
		{Key: SettingCaches, OwnedBy: "operator-computed", Impact: "drops the per-process template_fragments alias, the image-scoped KEY_PREFIX and the 1 s Memcached timeouts with ignore_exc; every page render then reads the Angular template fragment from the default alias, Horizon releases sharing one Memcached poison each other's template cache (OfflineGenerationError, HTTP 500), and a Memcached that stops answering blocks the dashboard"},
		{Key: SettingSessionEngine, OwnedBy: "operator-computed", Impact: sessionEngineImpact},
		{Key: SettingSessionCacheAlias, OwnedBy: "operator-computed", Impact: sessionCacheAliasImpact},
		{Key: "DEBUG", OwnedBy: "operator-computed"},
		{Key: "LOGGING", OwnedBy: "operator-computed"},
		{Key: "COMPRESS_OFFLINE", OwnedBy: "operator-computed", Impact: "static assets are pre-compressed at image build; disagreement breaks template rendering"},
		{Key: "STATIC_ROOT", OwnedBy: "operator-computed"},
		{Key: "WEBROOT", OwnedBy: "operator-computed"},
		{Key: "OPENSTACK_REQUEST_TIMEOUT", OwnedBy: "operator-computed", Impact: "the bound turns a service that stops answering into Horizon's own error after 30 seconds; without it a dashboard request waits out the kernel's retransmission limit, about 15 minutes, or Glance's 600 seconds, and holds the worker that serves it"},
		// Rejected: blocked at admission by the validating webhook.
		{Key: "SECRET_KEY", Rejected: true, OwnedBy: "spec.secretKeyRef"},
	}
	for _, name := range WebSSOSettingNames {
		keys = append(keys, config.OwnedKey{Key: name, Rejected: true, OwnedBy: "spec.websso"})
	}
	for _, name := range MultiDomainSettingNames {
		keys = append(keys, config.OwnedKey{Key: name, Rejected: true, OwnedBy: "spec.multiDomain"})
	}
	return keys
}
