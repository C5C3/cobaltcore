package policy

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"gopkg.in/yaml.v3"
)

// policyKey is the ConfigMap data key that holds the oslo.policy YAML content.
const policyKey = "policy.yaml"

// LoadPolicyFromConfigMap fetches a ConfigMap by name and namespace, reads the
// "policy.yaml" key from its data, and parses the YAML content into a flat
// map[string]string representing oslo.policy rules.
//
// Returns an error if the ConfigMap is not found, if the "policy.yaml" key is
// missing, or if the YAML content cannot be parsed. (CC-0005, REQ-008)
func LoadPolicyFromConfigMap(ctx context.Context, c client.Client, name, namespace string) (map[string]string, error) {
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, cm); err != nil {
		return nil, fmt.Errorf("fetching ConfigMap %s/%s: %w", namespace, name, err)
	}

	raw, ok := cm.Data[policyKey]
	if !ok {
		return nil, fmt.Errorf("ConfigMap %s/%s does not contain key %q", namespace, name, policyKey)
	}

	rules := make(map[string]string)
	if err := yaml.Unmarshal([]byte(raw), &rules); err != nil {
		return nil, fmt.Errorf("parsing %q from ConfigMap %s/%s: %w", policyKey, namespace, name, err)
	}

	return rules, nil
}
