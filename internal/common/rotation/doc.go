// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package rotation provides the split-compute-write credential-rotation
// mechanics shared by service operators: the staging Secret the rotation
// CronJob PATCHes, the operator-side commit that copies a validated staging
// payload onto the production Secret and deletes the staging Secret, the
// least-privilege ServiceAccount/Role/RoleBinding the CronJob runs under, the
// rotation-completed timestamp helpers, and the CronJob skeleton. The
// service-specific parts — the key kinds, the rotation script, the payload
// validation, and the event vocabulary — stay in the consuming operator.
package rotation
