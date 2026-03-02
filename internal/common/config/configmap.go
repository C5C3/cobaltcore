package config

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
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

	// Check if the ConfigMap already exists (idempotent).
	existing := &corev1.ConfigMap{}
	err := c.Get(ctx, types.NamespacedName{Name: hashedName, Namespace: namespace}, existing)
	if err == nil {
		return existing, nil
	}
	if client.IgnoreNotFound(err) != nil {
		// Real error (not "not found").
		return nil, fmt.Errorf("checking for existing ConfigMap %s/%s: %w", namespace, hashedName, err)
	}

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
		return nil, fmt.Errorf("creating ConfigMap %s/%s: %w", namespace, hashedName, err)
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
