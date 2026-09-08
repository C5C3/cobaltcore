// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package satellite provides the helpers shared by the satellite-CRD
// controllers and by the parent reconcilers that aggregate them. A satellite CR
// is a namespaced configuration CR attached to a parent service CR by name. It
// carries a credentials condition of its own that gates whether the parent may
// project it, and it observes its own effect through the Deployment the parent
// runs.
//
// The package has three parts. ResolveParentChildren reads the parent and hands
// back the client the satellite's observations belong on, holding the pass when
// the parent is absent or when the target cluster it names does not resolve.
// SecretNameForVolume, SectionPresent and SectionProjected read the projection
// back: the parent's Deployment, the Secret one of its volumes mounts, and the
// INI section header inside that Secret's data key. Collect turns a parent's
// list of attached satellites into an ordered, gated set, with the default
// candidates among them and the exactly-one-default decision.
//
// The consumers are GlanceBackend on every helper, BarbicanSecretStore on
// resolve, observe and collect, and KeystoneIdentityBackend on the volume lookup
// only.
//
// Keystone's satellite controller and its aggregation stay in the keystone
// operator by design. It observes the parent's Deployment on the management
// cluster, gates on the parent's KeystoneAPIReady instead of on a credentials
// condition of its own, observes a three-state projection behind a rollout gate,
// and preserves a True condition across transient reasons. A helper wide enough
// for all four would be a second copy of Keystone.
//
// The condition vocabulary, the messages, the events and the requeue intervals
// stay per operator. The helpers either hand a decision back for the operator to
// stamp, or take the condition texts as parameters.
package satellite
