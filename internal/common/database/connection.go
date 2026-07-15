// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package database

import (
	"context"
	"fmt"
	"net/url"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/forge/internal/common/conditions"
	commonv1 "github.com/c5c3/forge/internal/common/types"
)

const (
	// ConnectionSecretKey is the only Data key in the derived
	// <instance>-db-connection Secret. It mirrors the env override key used at
	// runtime (OS_DATABASE__CONNECTION) to keep the wiring obvious for the
	// deployment reconciler that consumes it.
	ConnectionSecretKey = "connection"

	// ConnectionEnvVarName is the oslo.config env override key for
	// [database].connection. The OS_<GROUP>__<OPTION> form wins over the
	// ConfigMap value at runtime, so service containers read the real DB URL from
	// the derived Secret instead of from the ConfigMap.
	ConnectionEnvVarName = "OS_DATABASE__CONNECTION"

	// ReasonWaitingForDBCredentials is the readiness-condition reason set while
	// the upstream (ESO-synced) credentials Secret or its required keys are
	// missing, so a derived Secret is never materialised with partial
	// credentials.
	ReasonWaitingForDBCredentials = "WaitingForDBCredentials"

	// TLSCAFileName / TLSCertFileName / TLSKeyFileName are the file names the
	// db-tls keypair is projected under in-pod. They are the default keys
	// cert-manager writes into the issued client Secret with default keyEncoding.
	// Both the ssl_ca/ssl_cert/ssl_key DSN parameters (TLSFilePaths) and the
	// workload volume projection reference these constants, so the DSN paths and
	// the mount layout stay in lockstep. If a future task overrides
	// cert-manager's secretTemplate keys, update these constants in lockstep.
	TLSCAFileName   = "ca.crt"
	TLSCertFileName = "tls.crt"
	TLSKeyFileName  = "tls.key"
)

// ConnectionSecretName returns the name of the derived DB-connection Secret for
// the given instance ("<instanceName>-db-connection").
func ConnectionSecretName(instanceName string) string {
	return instanceName + "-db-connection"
}

// ConnectionEnvVar returns the EnvVar that overrides [database].connection by
// sourcing the URL from the derived <instanceName>-db-connection Secret produced
// by ReconcileConnectionSecret. Every pod-spec builder that needs database
// access uses this helper to avoid string duplication.
func ConnectionEnvVar(instanceName string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: ConnectionEnvVarName,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: ConnectionSecretName(instanceName),
				},
				Key: ConnectionSecretKey,
			},
		},
	}
}

// TLSFilePaths returns the in-pod file paths consumed by the ssl_ca/ssl_cert/
// ssl_key DSN parameters when the db-tls Secret is mounted at mountPath.
// Centralising the layout here keeps the DSN assembly and the workload mount
// points in lockstep.
func TLSFilePaths(mountPath string) TLSPaths {
	return TLSPaths{
		CA:   mountPath + TLSCAFileName,
		Cert: mountPath + TLSCertFileName,
		Key:  mountPath + TLSKeyFileName,
	}
}

// ConnectionSecretFlowParams carries everything ReconcileConnectionSecret needs.
// The service-specific parts — the owner CR, the instance naming, the
// DatabaseSpec, the in-pod TLS mount path, and the condition vocabulary — are
// supplied by the caller; the read-assemble-materialise flow is identical across
// operators.
type ConnectionSecretFlowParams struct {
	Client client.Client
	Scheme *runtime.Scheme
	// Owner is the CR that owns the derived Secret.
	Owner client.Object
	// InstanceName drives the derived Secret name and the Static-mode username.
	InstanceName string
	// Namespace is the namespace of both the upstream and derived Secrets.
	Namespace string
	// Database is the shared DatabaseSpec (credentials Secret ref, host, TLS).
	Database *commonv1.DatabaseSpec
	// TLSMountPath is the in-pod directory the db-tls keypair is projected under;
	// the ssl_* DSN parameters reference files inside it.
	TLSMountPath string
	// Conditions is the CR's condition slice, mutated in place.
	Conditions *[]metav1.Condition
	// Generation is stamped onto every condition the flow writes.
	Generation int64
	// ConditionType is the readiness condition the flow reports on (for example
	// "SecretsReady").
	ConditionType string
	// RequeueAfter is the polling interval while the upstream credentials are not
	// yet available.
	RequeueAfter time.Duration
}

// ReconcileConnectionSecret derives the database connection URL from the upstream
// credentials Secret and writes it to the derived <instance>-db-connection
// Secret. When the upstream Secret or its required keys are missing it sets the
// readiness condition False with reason WaitingForDBCredentials and requeues; it
// never writes a derived Secret with empty credentials.
//
// It returns the SHA-256 digest of the assembled DSN so the deployment step can
// roll the Pods when a Dynamic (engine-issued) credential rotates without
// reading the Secret content itself; the digest is empty on the requeue/error
// paths where no derived Secret was materialised.
func ReconcileConnectionSecret(ctx context.Context, p ConnectionSecretFlowParams) (ctrl.Result, string, error) {
	upstreamKey := client.ObjectKey{
		Namespace: p.Namespace,
		Name:      p.Database.SecretRef.Name,
	}

	// waitForCredentials records the readiness condition False /
	// WaitingForDBCredentials and returns the polling requeue with an empty
	// digest, so a derived Secret is never materialised with partial credentials.
	waitForCredentials := func(msg string) (ctrl.Result, string, error) {
		conditions.SetCondition(p.Conditions, metav1.Condition{
			Type:               p.ConditionType,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: p.Generation,
			Reason:             ReasonWaitingForDBCredentials,
			Message:            msg,
		})
		return ctrl.Result{RequeueAfter: p.RequeueAfter}, "", nil
	}

	// Read the upstream credentials Secret exactly once so the username and
	// password are taken from a single, consistent object version. Reading each
	// half with a separate cache Get opens a window in which ESO's atomic Secret
	// update can land between the two reads, splicing one dynamic MySQL user's
	// username onto another user's password and yielding a DSN that can never
	// authenticate.
	secret := &corev1.Secret{}
	if err := p.Client.Get(ctx, upstreamKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return waitForCredentials(fmt.Sprintf("Upstream database credentials Secret %s/%s not found",
				upstreamKey.Namespace, upstreamKey.Name))
		}
		return ctrl.Result{}, "", fmt.Errorf("reading database credentials Secret %s/%s: %w",
			upstreamKey.Namespace, upstreamKey.Name, err)
	}

	// In Static managed mode the MariaDB User CR name (= InstanceName) is the
	// MySQL username, so it is not read from the Secret. Brownfield mode and
	// Dynamic managed mode both take "username" from the upstream Secret — the
	// dynamic engine issues an ephemeral username (e.g. v-kube-...) alongside the
	// password, so the username is not derivable from the CR name.
	username, ok := ResolveUsername(p.Database, p.InstanceName, secret.Data)
	if !ok {
		return waitForCredentials(fmt.Sprintf("Upstream database credentials Secret %s/%s missing key %q",
			upstreamKey.Namespace, upstreamKey.Name, "username"))
	}

	pw, ok := secret.Data["password"]
	if !ok {
		return waitForCredentials(fmt.Sprintf("Upstream database credentials Secret %s/%s missing key %q",
			upstreamKey.Namespace, upstreamKey.Name, "password"))
	}
	password := string(pw)

	// Build the query parameters via url.Values so the optional ssl_* keys
	// compose cleanly with the always-present charset=utf8. url.Values.Encode()
	// sorts keys lexically, yielding a deterministic, stable DSN across
	// reconciles.
	query := url.Values{}
	query.Set("charset", "utf8")
	if err := AppendTLSParams(p.Database.TLS, TLSFilePaths(p.TLSMountPath), query); err != nil {
		return ctrl.Result{}, "", fmt.Errorf("assembling database TLS DSN parameters: %w", err)
	}
	// BuildDSN assembles the pymysql URL (RFC 3986 userinfo encoding, literal "/"
	// preserved in the ssl_* query values so alembic's ConfigParser never sees
	// "%" interpolation syntax) and returns its rollout digest.
	connStr, digest := BuildDSN(username, password,
		ResolveHost(p.Database, p.Namespace), p.Database.Database, query)

	derivedKey := client.ObjectKey{
		Namespace: p.Namespace,
		Name:      ConnectionSecretName(p.InstanceName),
	}

	existing := &corev1.Secret{}
	err := p.Client.Get(ctx, derivedKey, existing)
	if apierrors.IsNotFound(err) {
		derived := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      derivedKey.Name,
				Namespace: derivedKey.Namespace,
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				ConnectionSecretKey: []byte(connStr),
			},
		}
		if err := controllerutil.SetControllerReference(p.Owner, derived, p.Scheme); err != nil {
			return ctrl.Result{}, "", fmt.Errorf("setting owner reference on derived Secret %s/%s: %w",
				derived.Namespace, derived.Name, err)
		}
		if err := p.Client.Create(ctx, derived); err != nil {
			return ctrl.Result{}, "", fmt.Errorf("creating derived Secret %s/%s: %w",
				derived.Namespace, derived.Name, err)
		}
		return ctrl.Result{}, digest, nil
	}
	if err != nil {
		return ctrl.Result{}, "", fmt.Errorf("getting derived Secret %s/%s: %w",
			derivedKey.Namespace, derivedKey.Name, err)
	}

	// The derived Secret must contain exactly the one "connection" key; replace
	// Data wholesale on any drift (value change OR extra keys present).
	current, ok := existing.Data[ConnectionSecretKey]
	if len(existing.Data) != 1 || !ok || string(current) != connStr {
		existing.Data = map[string][]byte{
			ConnectionSecretKey: []byte(connStr),
		}
		if err := p.Client.Update(ctx, existing); err != nil {
			return ctrl.Result{}, "", fmt.Errorf("updating derived Secret %s/%s: %w",
				existing.Namespace, existing.Name, err)
		}
	}

	return ctrl.Result{}, digest, nil
}
