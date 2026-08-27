// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// conditionTypeOVNEndpointsReady is the condition both OVN steps report under.
// It is the gate the config step waits behind: the two connection strings and
// the client certificate parameterise the [ovn] section, so nothing is rendered
// while it is False.
const conditionTypeOVNEndpointsReady = "OVNEndpointsReady"

// The condition reasons of the two OVN steps.
const (
	conditionReasonOVNCentralNotFound          = "OVNCentralNotFound"
	conditionReasonOVNCentralReadError         = "OVNCentralReadError"
	conditionReasonOVNEndpointsPending         = "OVNEndpointsPending"
	conditionReasonOVNEndpointsResolved        = "OVNEndpointsResolved"
	conditionReasonOVNClientSecretPending      = "OVNClientSecretPending"
	conditionReasonOVNClientSecretIncomplete   = "OVNClientSecretIncomplete"
	conditionReasonOVNClientSecretReadError    = "OVNClientSecretReadError"
	conditionReasonOVNClientSecretMirrorFailed = "OVNClientSecretMirrorFailed"
)

// ovnClientSecretSuffix is appended to metadata.name to name the mirrored client
// Secret the Neutron pods mount.
const ovnClientSecretSuffix = "-ovn-client"

// ovnClientSecretKeys are the three data keys an OVN client identity consists
// of, in the order the missing-key check reports them: the keypair first, the CA
// bundle that verifies the database endpoint last.
var ovnClientSecretKeys = []string{"tls.crt", "tls.key", "ca.crt"}

// ovnClientSecretName returns the name of the mirrored client Secret for the
// given Neutron.
func ovnClientSecretName(neutron *neutronv1alpha1.Neutron) string {
	return neutron.Name + ovnClientSecretSuffix
}

// resolvedOVNEndpoints carries what the config and deployment steps need from
// the OVNCentral this Neutron drives: the two database addresses the ML2/OVN
// mechanism driver dials, and the central CR itself, which the client-Secret
// step reads the source Secret from.
type resolvedOVNEndpoints struct {
	nbAddress string
	sbAddress string
	central   *ovnv1alpha1.OVNCentral
}

// reconcileOVNEndpoints resolves the Northbound and Southbound addresses of the
// OVNCentral named by spec.ovn.centralRef.
//
// The central CR is read through the management-cluster client rather than
// through the children client: both CRs live on the management cluster whatever
// cluster their children land on, and the ref carries a namespace, because the
// OVN control plane commonly lives in the privileged networking namespace while
// the Neutron API lives with the rest of the control plane.
func (r *NeutronReconciler) reconcileOVNEndpoints(ctx context.Context, neutron *neutronv1alpha1.Neutron) (resolvedOVNEndpoints, ctrl.Result, error) {
	namespace := neutron.Spec.OVN.CentralRef.Namespace
	if namespace == "" {
		namespace = neutron.Namespace
	}
	name := neutron.Spec.OVN.CentralRef.Name

	central := &ovnv1alpha1.OVNCentral{}
	switch err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, central); {
	case apierrors.IsNotFound(err):
		// A Neutron applied before its OVNCentral is an ordinary ordering of two
		// objects in one manifest, so this polls rather than failing the pass.
		markOVNEndpoints(neutron, metav1.ConditionFalse, conditionReasonOVNCentralNotFound,
			fmt.Sprintf("OVNCentral %s/%s does not exist; the ML2/OVN mechanism driver stays "+
				"unconfigured until it does", namespace, name))
		return resolvedOVNEndpoints{}, ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
	case err != nil:
		err = fmt.Errorf("reading OVNCentral %s/%s: %w", namespace, name, err)
		markOVNEndpoints(neutron, metav1.ConditionFalse, conditionReasonOVNCentralReadError, err.Error())
		return resolvedOVNEndpoints{}, ctrl.Result{}, err
	}

	// Which pair of published addresses applies follows from where the two CRs
	// project their children. Inside one cluster the databases are reached at
	// their Service addresses; from another cluster only the node ports the
	// central publishes for an externally reachable database are routable.
	sameCluster := sameTargetCluster(neutron.Spec.TargetClusterRef, central.Spec.TargetClusterRef)
	nbAddress, sbAddress := central.Status.Northbound.InternalDbAddress, central.Status.Southbound.InternalDbAddress
	if !sameCluster {
		nbAddress, sbAddress = central.Status.Northbound.DbAddress, central.Status.Southbound.DbAddress
	}

	if nbAddress == "" || sbAddress == "" {
		markOVNEndpoints(neutron, metav1.ConditionFalse, conditionReasonOVNEndpointsPending,
			pendingAddressMessage(neutron, central, namespace, name, sameCluster))
		return resolvedOVNEndpoints{}, ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
	}
	if central.Status.ClientSecretName == "" {
		markOVNEndpoints(neutron, metav1.ConditionFalse, conditionReasonOVNEndpointsPending,
			fmt.Sprintf("Waiting for OVNCentral %s/%s to publish its client Secret; the databases "+
				"accept no connection without a client certificate", namespace, name))
		return resolvedOVNEndpoints{}, ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
	}

	markOVNEndpoints(neutron, metav1.ConditionTrue, conditionReasonOVNEndpointsResolved,
		fmt.Sprintf("The ML2/OVN mechanism driver connects to OVNCentral %s/%s at %s (Northbound) "+
			"and %s (Southbound)", namespace, name, nbAddress, sbAddress))
	return resolvedOVNEndpoints{nbAddress: nbAddress, sbAddress: sbAddress, central: central}, ctrl.Result{}, nil
}

// pendingAddressMessage explains which address is missing in terms of the field
// that publishes it. Across a cluster boundary that is spec.northbound and
// spec.southbound externallyReachable on the OVNCentral: the databases carry no
// address outside their own cluster until they are published on node ports.
func pendingAddressMessage(neutron *neutronv1alpha1.Neutron, central *ovnv1alpha1.OVNCentral,
	namespace, name string, sameCluster bool,
) string {
	if sameCluster {
		return fmt.Sprintf("Waiting for OVNCentral %s/%s to publish its Northbound and Southbound addresses",
			namespace, name)
	}
	return fmt.Sprintf("OVNCentral %s/%s projects onto %s while this Neutron projects onto %s, so it is "+
		"reached at the addresses published outside its cluster; set spec.northbound.externallyReachable "+
		"and spec.southbound.externallyReachable on the OVNCentral to true to publish them",
		namespace, name, describeTargetCluster(central.Spec.TargetClusterRef),
		describeTargetCluster(neutron.Spec.TargetClusterRef))
}

// reconcileOVNClientSecret mirrors the client identity the OVNCentral publishes
// into the Neutron's own namespace, as <neutron.Name>-ovn-client, and returns
// the SHA-256 digest of its three values so the deployment step can roll the
// pods when the certificate is reissued.
//
// The mirror exists because the source Secret is not reachable where the pods
// are: it lives in the central's namespace, and on a placed Neutron even on
// another cluster. The source is read live through the central's own children
// client, so an ownership decision is never made on a cache that has not caught
// up; the mirror is written through this Neutron's children client, which is
// what makes it mountable.
//
// Every failure arm overwrites OVNEndpointsReady, the condition the endpoint
// step left True: a resolved address the pods cannot authenticate against is not
// a usable endpoint.
func (r *NeutronReconciler) reconcileOVNClientSecret(ctx context.Context, children client.Client,
	neutron *neutronv1alpha1.Neutron, ovn resolvedOVNEndpoints,
) (string, ctrl.Result, error) {
	centralChildren, err := commonmulticluster.ResolveChildrenClient(ctx, r.Resolver, r.Client,
		ovn.central.Spec.TargetClusterRef)
	if err != nil {
		markOVNEndpoints(neutron, metav1.ConditionFalse, commonmulticluster.TargetClusterUnavailable, err.Error())
		return "", ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
	}

	sourceKey := client.ObjectKey{Namespace: ovn.central.Namespace, Name: ovn.central.Status.ClientSecretName}
	source := &corev1.Secret{}
	switch err := commonmulticluster.LiveReader(centralChildren).Get(ctx, sourceKey, source); {
	case apierrors.IsNotFound(err):
		// The central publishes the name in its status before cert-manager has
		// issued the certificate, so an absent Secret is an ordinary wait.
		markOVNEndpoints(neutron, metav1.ConditionFalse, conditionReasonOVNClientSecretPending,
			fmt.Sprintf("Waiting for the OVN client Secret %s/%s the OVNCentral publishes",
				sourceKey.Namespace, sourceKey.Name))
		return "", ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
	case err != nil:
		err = fmt.Errorf("reading OVN client Secret %s/%s: %w", sourceKey.Namespace, sourceKey.Name, err)
		markOVNEndpoints(neutron, metav1.ConditionFalse, conditionReasonOVNClientSecretReadError, err.Error())
		return "", ctrl.Result{}, err
	}

	data := make(map[string][]byte, len(ovnClientSecretKeys))
	for _, key := range ovnClientSecretKeys {
		value := source.Data[key]
		if len(value) == 0 {
			markOVNEndpoints(neutron, metav1.ConditionFalse, conditionReasonOVNClientSecretIncomplete,
				fmt.Sprintf("OVN client Secret %s/%s carries no %s yet", sourceKey.Namespace, sourceKey.Name, key))
			return "", ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
		}
		data[key] = value
	}

	if err := r.writeOVNClientSecret(ctx, children, neutron, data); err != nil {
		markOVNEndpoints(neutron, metav1.ConditionFalse, conditionReasonOVNClientSecretMirrorFailed, err.Error())
		return "", ctrl.Result{}, err
	}
	return ovnClientSecretDigest(data), ctrl.Result{}, nil
}

// writeOVNClientSecret creates the mirrored Secret or repairs it, so the copy
// carries exactly the three source values and nothing else: a stale certificate
// left behind by a reissue would authenticate against nothing, and an extra key
// would survive in a Secret the operator owns.
func (r *NeutronReconciler) writeOVNClientSecret(ctx context.Context, children client.Client,
	neutron *neutronv1alpha1.Neutron, data map[string][]byte,
) error {
	key := client.ObjectKey{Namespace: neutron.Namespace, Name: ovnClientSecretName(neutron)}

	mirror := &corev1.Secret{}
	switch err := children.Get(ctx, key, mirror); {
	case apierrors.IsNotFound(err):
		mirror = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			Type:       corev1.SecretTypeOpaque,
			Data:       data,
		}
		if cerr := commonmulticluster.Claim(children, r.Scheme, neutron, mirror); cerr != nil {
			return fmt.Errorf("claiming mirrored OVN client Secret %s/%s: %w", key.Namespace, key.Name, cerr)
		}
		if cerr := children.Create(ctx, mirror); cerr != nil {
			return fmt.Errorf("creating mirrored OVN client Secret %s/%s: %w", key.Namespace, key.Name, cerr)
		}
		return nil
	case err != nil:
		return fmt.Errorf("reading mirrored OVN client Secret %s/%s: %w", key.Namespace, key.Name, err)
	}

	if maps.EqualFunc(mirror.Data, data, bytes.Equal) {
		return nil
	}
	mirror.Data = data
	if err := children.Update(ctx, mirror); err != nil {
		return fmt.Errorf("updating mirrored OVN client Secret %s/%s: %w", key.Namespace, key.Name, err)
	}
	return nil
}

// ovnClientSecretDigest returns the SHA-256 of the mirrored values as a
// lowercase hex string. The entries are hashed in sorted key order and
// length-prefixed, the encoding config.CreateImmutableConfigMap hashes its data
// with, so a value that ends where the next one begins cannot collide with the
// pair that swaps that boundary.
func ovnClientSecretDigest(data map[string][]byte) string {
	h := sha256.New()
	for _, k := range slices.Sorted(maps.Keys(data)) {
		_, _ = fmt.Fprintf(h, "%d:%s=%d:%s\n", len(k), k, len(data[k]), data[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// markOVNEndpoints writes OVNEndpointsReady at the CR's generation. Both OVN
// steps report through it, so the condition has exactly one write site.
func markOVNEndpoints(neutron *neutronv1alpha1.Neutron, status metav1.ConditionStatus, reason, message string) {
	conditions.SetCondition(&neutron.Status.Conditions, metav1.Condition{
		Type:               conditionTypeOVNEndpointsReady,
		Status:             status,
		ObservedGeneration: neutron.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// sameTargetCluster reports whether two CRs project their children onto the
// same cluster. Two nil refs both mean the management cluster; two set refs
// agree when they name the same registered cluster.
func sameTargetCluster(a, b *commonv1.TargetClusterRefSpec) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Name == b.Name
}

// describeTargetCluster names the cluster a ref selects, for a condition message
// a reader can act on. A nil ref is the cluster the operator itself runs in.
func describeTargetCluster(ref *commonv1.TargetClusterRefSpec) string {
	if ref == nil {
		return "the management cluster"
	}
	return "target cluster " + ref.Name
}
