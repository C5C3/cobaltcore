// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bytes"
	"context"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	"github.com/c5c3/cobaltcore/internal/common/naming"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// The condition reasons of the client-Secret step. It reports under
// CentralReady, the condition the central step left True, because a central the
// chassis can reach but not authenticate against is not a usable central.
const (
	conditionReasonClientSecretPending    = "ClientSecretPending"
	conditionReasonClientSecretIncomplete = "ClientSecretIncomplete"
	conditionReasonClientSecretReadError  = "ClientSecretReadError"
	conditionReasonClientSecretCopyFailed = "ClientSecretCopyFailed"
)

// componentClientSecret is the component-label value and the name suffix of the
// client-identity copy.
const componentClientSecret = "ovn-client"

// ovnClientSecretKeys are the three data keys an OVN client identity consists
// of, in the order the missing-key check reports them: the keypair first, the CA
// bundle that verifies the database endpoint last.
var ovnClientSecretKeys = []string{"tls.crt", "tls.key", "ca.crt"}

// chassisClientSecretName names the copy of the central's client Secret on the
// chassis's cluster. The webhook bounds the chassis name at
// MaxOVNChassisNameLength (42), which leaves the suffix room under the
// 63-character bound of a name recorded as an ownership label.
func chassisClientSecretName(cr *ovnv1alpha1.OVNChassis) string {
	return cr.Name + "-" + componentClientSecret
}

// reconcileClientSecret settles which Secret the chassis pods mount, and returns
// its name.
//
// A chassis on its central's cluster mounts the Secret the central publishes, as
// it always has, and nothing is written. A chassis on another cluster cannot: a
// Secret does not cross a cluster boundary, so the step copies the client
// identity into <chassis>-ovn-client in the chassis's namespace on the chassis's
// cluster. The source is read live through the central's own children client,
// so a renewal is never judged from a cache that has not caught up; the copy is
// written through this chassis's children client, which is what makes it
// mountable where the pods run.
//
// No pod rolls when the copy changes. ovn-controller and the maintenance Jobs
// read the key, the certificate and the CA from the mounted files, which the
// kubelet refreshes in place, the same way a certificate renewal reaches a
// chassis that mounts the central's Secret directly.
//
// Every failure arm overwrites CentralReady, the condition the central step
// left True.
func (r *OVNChassisReconciler) reconcileClientSecret(ctx context.Context, children client.Client,
	cr *ovnv1alpha1.OVNChassis, central resolvedCentral,
) (string, ctrl.Result, error) {
	if central.sameCluster {
		cr.Status.ClientSecretName = central.sourceClientSecretName
		return central.sourceClientSecretName, ctrl.Result{}, nil
	}

	centralChildren, err := commonmulticluster.ResolveChildrenClient(ctx, r.Resolver, r.Client,
		central.centralTargetClusterRef)
	if err != nil {
		markClientSecret(cr, commonmulticluster.TargetClusterUnavailable, err.Error())
		return "", ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
	}

	sourceKey := client.ObjectKey{Namespace: cr.Namespace, Name: central.sourceClientSecretName}
	source := &corev1.Secret{}
	switch err := commonmulticluster.LiveReader(centralChildren).Get(ctx, sourceKey, source); {
	case apierrors.IsNotFound(err):
		// The central publishes the name in its status before cert-manager has
		// issued the certificate, so an absent Secret is an ordinary wait.
		markClientSecret(cr, conditionReasonClientSecretPending,
			fmt.Sprintf("Waiting for the OVN client Secret %s/%s the OVNCentral publishes",
				sourceKey.Namespace, sourceKey.Name))
		return "", ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
	case err != nil:
		err = fmt.Errorf("reading OVN client Secret %s/%s: %w", sourceKey.Namespace, sourceKey.Name, err)
		markClientSecret(cr, conditionReasonClientSecretReadError, err.Error())
		return "", ctrl.Result{}, err
	}

	data := make(map[string][]byte, len(ovnClientSecretKeys))
	for _, key := range ovnClientSecretKeys {
		value := source.Data[key]
		if len(value) == 0 {
			markClientSecret(cr, conditionReasonClientSecretIncomplete,
				fmt.Sprintf("OVN client Secret %s/%s carries no %s yet", sourceKey.Namespace, sourceKey.Name, key))
			return "", ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
		}
		data[key] = value
	}

	if err := r.writeChassisClientSecret(ctx, children, cr, data); err != nil {
		markClientSecret(cr, conditionReasonClientSecretCopyFailed, err.Error())
		return "", ctrl.Result{}, err
	}

	cr.Status.ClientSecretName = chassisClientSecretName(cr)
	return cr.Status.ClientSecretName, ctrl.Result{}, nil
}

// writeChassisClientSecret creates the copy or repairs it, so it carries exactly
// the three source values and nothing else: a stale certificate left behind by
// a renewal would authenticate against nothing, and an extra key would survive
// in a Secret the operator owns.
//
// A Secret of that name the chassis does not own is refused rather than
// overwritten. It holds whatever somebody else put there, and replacing its data
// with a client key would hand that key to whoever reads it.
func (r *OVNChassisReconciler) writeChassisClientSecret(ctx context.Context, children client.Client,
	cr *ovnv1alpha1.OVNChassis, data map[string][]byte,
) error {
	key := client.ObjectKey{Namespace: cr.Namespace, Name: chassisClientSecretName(cr)}

	existing := &corev1.Secret{}
	switch err := children.Get(ctx, key, existing); {
	case apierrors.IsNotFound(err):
		copied := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      key.Name,
				Namespace: key.Namespace,
				Labels:    naming.ComponentLabels(chassisAppName, cr.Name, componentClientSecret),
			},
			Type: corev1.SecretTypeOpaque,
			Data: data,
		}
		if cerr := commonmulticluster.Claim(children, r.Scheme, cr, copied); cerr != nil {
			return fmt.Errorf("claiming the OVN client Secret copy %s/%s: %w", key.Namespace, key.Name, cerr)
		}
		if cerr := children.Create(ctx, copied); cerr != nil {
			return fmt.Errorf("creating the OVN client Secret copy %s/%s: %w", key.Namespace, key.Name, cerr)
		}
		return nil
	case err != nil:
		return fmt.Errorf("reading the OVN client Secret copy %s/%s: %w", key.Namespace, key.Name, err)
	}

	owned, err := commonmulticluster.Controls(r.Scheme, cr, existing)
	if err != nil {
		return fmt.Errorf("checking ownership of the OVN client Secret copy %s/%s: %w", key.Namespace, key.Name, err)
	}
	if !owned {
		return fmt.Errorf("refusing to overwrite Secret %s/%s: it exists and is not owned by this OVNChassis",
			key.Namespace, key.Name)
	}

	if maps.EqualFunc(existing.Data, data, bytes.Equal) {
		return nil
	}
	existing.Data = data
	if err := children.Update(ctx, existing); err != nil {
		return fmt.Errorf("updating the OVN client Secret copy %s/%s: %w", key.Namespace, key.Name, err)
	}
	return nil
}

// markClientSecret writes CentralReady=False at the CR's generation for one of
// the client-Secret step's failure arms.
func markClientSecret(cr *ovnv1alpha1.OVNChassis, reason, message string) {
	conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
		Type:               conditionTypeCentralReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: cr.Generation,
		Reason:             reason,
		Message:            message,
	})
}
