// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package multicluster

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// The ownership labels are the target-cluster substitute for a controller owner
// reference. A reference is resolved by the garbage collector of the cluster the
// child lives in, and a CR that names a target cluster does not live there: on
// the target the reference names a UID that is absent, so it either means
// nothing at all or — once the owner's kind is registered on that cluster too —
// gets the child collected as an orphan. A child written to a target cluster
// therefore carries no owner reference and is stamped with these three labels
// instead, which name its owner unambiguously.
//
// They carry the two jobs a reference would have done, and the operator does
// both by hand because nothing on the target cluster does them for it:
//
//   - RECOGNITION. Controls answers "may this CR write to, and delete, this
//     object?" — from the owner reference for a local child, from these labels
//     for a remote one. An object carrying neither is nobody's child and is left
//     alone.
//   - CLEANUP. No garbage collection cascade reaches a remote child, so the CR's
//     teardown deletes them explicitly, held open by RemoteChildrenFinalizer.
//
// The kind belongs in the vocabulary because name and namespace do not identify
// an owner on their own: a Keystone and a Barbican both called "openstack" in
// namespace "openstack" project children into the same target namespace, and
// each must select only its own.
const (
	OwnerKindLabel      = "openstack.c5c3.io/owner-kind"
	OwnerNameLabel      = "openstack.c5c3.io/owner-name"
	OwnerNamespaceLabel = "openstack.c5c3.io/owner-namespace"
)

// MaxOwnerNameLength is the longest metadata.name a CR that projects onto a
// target cluster may carry. Its name is recorded as a label VALUE, which
// Kubernetes caps at 63 characters, while metadata.name is a DNS-1123 subdomain
// capped at 253. Claim refuses a longer name rather than letting every child
// write fail deep inside the API server on an invalid label value.
const MaxOwnerNameLength = validation.LabelValueMaxLength

// RemoteChildrenFinalizer is the finalizer a CR carries while children of it
// exist on a target cluster. Nothing on that cluster deletes them when the CR
// goes, so the operator has to, and the finalizer is what keeps the CR in etcd
// long enough to do it. It is shared across the service operators: the children
// they leave behind differ, the reason the CR has to outlive its own deletion
// does not.
const RemoteChildrenFinalizer = "openstack.c5c3.io/remote-children"

// OwnerLabels returns the ownership labels identifying owner as the owner of a
// remote child.
//
// The kind is resolved through the scheme rather than read off the object: a
// typed CR built in code carries an empty TypeMeta, and the typed client does
// not populate it on Get either, so reading the kind off the object would stamp
// an empty label for nearly every caller.
func OwnerLabels(scheme *runtime.Scheme, owner client.Object) (map[string]string, error) {
	gvk, err := ownerGVK(scheme, owner)
	if err != nil {
		return nil, err
	}
	return ownerLabelsFor(gvk, owner), nil
}

// Remote marks c as the client of a target cluster, so Claim can tell the two
// kinds of client apart. ResolveChildrenClient applies the mark; the local
// client is handed out unmarked, which is what keeps single-cluster behavior
// byte-identical.
//
// A client marked here carries no API reader, so LiveReader falls back to c
// itself. ResolveChildrenClient, which has the resolved cluster in hand, marks
// with the reader.
func Remote(c client.Client) client.Client {
	return remoteClient{Client: c}
}

// IsRemote reports whether c writes to a target cluster, that is, whether it
// came out of Remote.
func IsRemote(c client.Client) bool {
	_, remote := c.(remoteClient)
	return remote
}

// LiveReader returns the reader an ownership decision about c's cluster has to
// be made from: the target cluster's own uncached API reader when c is a
// resolved remote client, and c itself otherwise.
//
// Two things follow from reading live state rather than through c's cache. An
// object written to the target moments ago cannot look absent and be adopted on
// the strength of a cache that has not caught up. And a kind the operator only
// ever writes — a NetworkPolicy, an HTTPRoute, a PodDisruptionBudget — is not
// pulled into an informer by the read, which on a cluster-scoped install would
// start an all-namespaces LIST+WATCH of that kind on the target and hold its
// every object resident for the lifetime of the process.
func LiveReader(c client.Client) client.Reader {
	if remote, marked := c.(remoteClient); marked && remote.reader != nil {
		return remote.reader
	}
	return c
}

// Controls reports whether owner owns obj: either it is obj's controller owner
// reference (the local case), or obj carries owner's ownership labels (the
// remote case, where no usable reference exists). It is the ownership test
// every write and every delete against a target cluster gates on, so an object
// somebody else provisioned under a name one of our children happens to want is
// never reshaped and never deleted.
func Controls(scheme *runtime.Scheme, owner, obj client.Object) (bool, error) {
	labels, err := OwnerLabels(scheme, owner)
	if err != nil {
		return false, err
	}
	return controls(owner, obj, labels), nil
}

// Claim makes obj a child of owner by the only mechanism the child's cluster
// permits: a controller owner reference when c writes to the local cluster (so
// the garbage collection cascade reaps it), the ownership labels when c is a
// resolved target-cluster client (so the teardown can find and delete it). It is
// the single decision point, so no projection site has to re-derive which
// mechanism applies to the client it was handed.
//
// The local path is controllerutil.SetControllerReference and nothing else, its
// error included: a CR that names no target cluster is claimed exactly as it
// always was.
//
// The remote path never sets a reference. It drops one naming owner if it finds
// it, because on the target cluster such a reference names an absent UID and
// leaves the child one kind registration away from being collected as an orphan.
// A live object (its UID is set, so it was read back from the API server) that
// is neither referenced nor labelled is refused rather than adopted: adopting it
// would be doubly destructive, since the apply overwrites its spec and the
// labels stamped on it get it deleted at teardown. Nothing is written to obj in
// that case.
func Claim(c client.Client, scheme *runtime.Scheme, owner, obj client.Object) error {
	if !IsRemote(c) {
		return controllerutil.SetControllerReference(owner, obj, scheme)
	}

	gvk, err := ownerGVK(scheme, owner)
	if err != nil {
		return err
	}
	labels := ownerLabelsFor(gvk, owner)

	if len(owner.GetName()) > MaxOwnerNameLength {
		return fmt.Errorf("refusing to claim %s %s/%s for %s %s: the owner's name is %d characters, and a name a "+
			"child records as an ownership label may not exceed %d",
			kindOf(scheme, obj), obj.GetNamespace(), obj.GetName(), gvk.Kind, owner.GetName(),
			len(owner.GetName()), MaxOwnerNameLength)
	}

	if obj.GetUID() != "" && !controls(owner, obj, labels) {
		return fmt.Errorf("refusing to adopt pre-existing %s %s/%s: it was not created by this %s, so adopting it "+
			"would overwrite its spec and delete it at teardown",
			kindOf(scheme, obj), obj.GetNamespace(), obj.GetName(), gvk.Kind)
	}

	dropControllerRef(obj, gvk, owner)
	stampLabels(obj, labels)
	return nil
}

// remoteClient is the mark Remote puts on a target-cluster client. It adds no
// behavior: every call goes to the embedded client, and the type exists only so
// Claim can recognize which cluster it is claiming on.
//
// reader is that cluster's uncached API reader, which LiveReader hands out and
// nothing else reads. It is nil for a client marked by Remote alone, which is
// every client no cluster was resolved for.
type remoteClient struct {
	client.Client
	reader client.Reader
}

// ownerGVK resolves owner's kind through the scheme, under the error message
// every entry point in this file reports that failure with.
func ownerGVK(scheme *runtime.Scheme, owner client.Object) (schema.GroupVersionKind, error) {
	gvk, err := apiutil.GVKForObject(owner, scheme)
	if err != nil {
		return schema.GroupVersionKind{}, fmt.Errorf("resolving GVK for owner %s/%s: %w",
			owner.GetNamespace(), owner.GetName(), err)
	}
	return gvk, nil
}

// ownerLabelsFor builds the ownership labels from an already-resolved owner
// kind, so a caller that needs the kind for something else does not pay for a
// second scheme lookup.
func ownerLabelsFor(gvk schema.GroupVersionKind, owner client.Object) map[string]string {
	return map[string]string{
		OwnerKindLabel:      gvk.Kind,
		OwnerNameLabel:      owner.GetName(),
		OwnerNamespaceLabel: owner.GetNamespace(),
	}
}

// controls is Controls against owner's already-resolved labels.
func controls(owner, obj client.Object, ownerLabels map[string]string) bool {
	if metav1.IsControlledBy(obj, owner) {
		return true
	}
	labels := obj.GetLabels()
	for key, value := range ownerLabels {
		if labels[key] != value {
			return false
		}
	}
	return true
}

// kindOf names obj for an error message, from the scheme where it can and from
// obj's own TypeMeta otherwise, which is how an unstructured object still gets
// named. A kind no scheme knows leaves the name empty, and the namespace and
// name in the same message still identify the object.
func kindOf(scheme *runtime.Scheme, obj client.Object) string {
	if gvk, err := apiutil.GVKForObject(obj, scheme); err == nil {
		return gvk.Kind
	}
	return obj.GetObjectKind().GroupVersionKind().Kind
}

// dropControllerRef removes a controller owner reference naming owner from obj.
// It matches on group, kind and name rather than on UID, so it also reaches the
// reference a child acquired from a management-cluster owner whose UID means
// nothing on the target cluster. Every other reference is left where it is.
func dropControllerRef(obj client.Object, gvk schema.GroupVersionKind, owner client.Object) {
	refs := obj.GetOwnerReferences()
	kept := make([]metav1.OwnerReference, 0, len(refs))
	for _, ref := range refs {
		if !namesOwner(ref, gvk, owner) {
			kept = append(kept, ref)
		}
	}
	if len(kept) != len(refs) {
		obj.SetOwnerReferences(kept)
	}
}

// namesOwner reports whether ref is a controller reference to owner.
func namesOwner(ref metav1.OwnerReference, gvk schema.GroupVersionKind, owner client.Object) bool {
	if ref.Controller == nil || !*ref.Controller || ref.Kind != gvk.Kind || ref.Name != owner.GetName() {
		return false
	}
	group, err := schema.ParseGroupVersion(ref.APIVersion)
	return err == nil && group.Group == gvk.Group
}

// stampLabels merges the ownership labels onto obj's own labels, preserving the
// ones already there. It touches the top-level labels only: a workload's pod
// template selects on labels of its own, and rewriting those would orphan the
// pods the workload already runs.
func stampLabels(obj client.Object, ownerLabels map[string]string) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	for key, value := range ownerLabels {
		labels[key] = value
	}
	obj.SetLabels(labels)
}
