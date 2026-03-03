package config

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CreateImmutableConfigMap creates an immutable Kubernetes ConfigMap whose name
// includes a content-hash suffix derived from the data. If a ConfigMap with the
// same hashed name already exists, it is returned without error (idempotent).
//
// The name is formatted as "{name}-{hash}" where hash is the first 8 hex
// characters of the SHA-256 digest of the sorted, serialised data content.
//
// The operation uses a create-first approach: it attempts to create the
// resource, and only if creation fails with AlreadyExists does it fetch and
// return the existing resource. This avoids a redundant lookup in the common
// case where the resource does not yet exist.
//
// Owner references are set from the variadic owners parameter so the ConfigMap
// is garbage-collected when the owning resource is deleted. (CC-0005, REQ-001, REQ-009, REQ-010)
func CreateImmutableConfigMap(
	ctx context.Context,
	c client.Client,
	name, namespace string,
	data map[string]string,
	owners ...metav1.OwnerReference,
) (*corev1.ConfigMap, error) {
	hashedName := fmt.Sprintf("%s-%s", name, contentHash(data))

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            hashedName,
			Namespace:       namespace,
			OwnerReferences: owners,
		},
		Immutable: ptr.To(true),
		Data:      data,
	}

	if err := c.Create(ctx, cm); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("creating ConfigMap %s/%s: %w", namespace, hashedName, err)
		}
		// Already exists — fetch and return the existing resource.
		existing := &corev1.ConfigMap{}
		if err := c.Get(ctx, types.NamespacedName{Name: hashedName, Namespace: namespace}, existing); err != nil {
			return nil, fmt.Errorf("getting existing ConfigMap %s/%s: %w", namespace, hashedName, err)
		}
		return existing, nil
	}

	return cm, nil
}

// contentHash computes a short content hash (8 hex characters) from sorted
// key=value pairs of the given data map.
func contentHash(data map[string]string) string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, data[k])
	}
	sum := sha256.Sum256([]byte(b.String()))
	return fmt.Sprintf("%x", sum[:4]) // 8 hex chars
}
