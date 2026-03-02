package policy

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// LoadPolicyFromConfigMap fetches a ConfigMap and parses the 'policy.yaml' key
// into a map[string]string of policy rules. (CC-0005, REQ-008)
func LoadPolicyFromConfigMap(ctx context.Context, c client.Client, namespace, name string) (map[string]string, error) {
	var cm corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &cm); err != nil {
		return nil, fmt.Errorf("fetching ConfigMap %s/%s: %w", namespace, name, err)
	}

	raw, ok := cm.Data["policy.yaml"]
	if !ok {
		return nil, fmt.Errorf("ConfigMap %s/%s does not contain key \"policy.yaml\"", namespace, name)
	}

	var rules map[string]string
	if err := yaml.Unmarshal([]byte(raw), &rules); err != nil {
		return nil, fmt.Errorf("parsing policy.yaml from ConfigMap %s/%s: %w", namespace, name, err)
	}

	return rules, nil
}

// MergePolicies merges inline policy rules over external rules with inline-wins precedence.
// Returns a new map without mutating inputs. (CC-0004, REQ-011)
func MergePolicies(external, inline map[string]string) map[string]string {
	result := make(map[string]string)

	for k, v := range external {
		result[k] = v
	}
	for k, v := range inline {
		result[k] = v
	}

	return result
}

// ValidatePolicyRules validates policy rules and returns a field.ErrorList for webhook compatibility.
// Each rule must have a non-empty key and non-empty value. (CC-0004, REQ-012)
func ValidatePolicyRules(rules map[string]string, fldPath *field.Path) field.ErrorList {
	var errs field.ErrorList

	// Sort keys for deterministic error ordering.
	keys := make([]string, 0, len(rules))
	for k := range rules {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		v := rules[k]
		if k == "" {
			errs = append(errs, field.Required(fldPath, "policy rule key must not be empty"))
		}
		if v == "" {
			errs = append(errs, field.Invalid(fldPath.Key(k), v, "policy rule value must not be empty"))
		}
	}

	return errs
}

// RenderPolicyYAML renders policy rules as a YAML document suitable for oslo.policy.
// Returns valid YAML. Empty map returns empty YAML ("{}\n"). (CC-0004, REQ-013)
func RenderPolicyYAML(rules map[string]string) (string, error) {
	if len(rules) == 0 {
		return "{}\n", nil
	}

	// Sort keys for deterministic output.
	keys := make([]string, 0, len(rules))
	for k := range rules {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Build an ordered map using yaml.Node for sorted key output.
	mapping := &yaml.Node{
		Kind: yaml.MappingNode,
		Tag:  "!!map",
	}
	for _, k := range keys {
		mapping.Content = append(mapping.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: k, Tag: "!!str"},
			&yaml.Node{Kind: yaml.ScalarNode, Value: rules[k], Tag: "!!str"},
		)
	}

	doc := &yaml.Node{
		Kind:    yaml.DocumentNode,
		Content: []*yaml.Node{mapping},
	}

	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", err
	}

	return string(out), nil
}
