package policy

import (
	"testing"

	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func TestMergePolicies_InlineWinsOnCollision(t *testing.T) {
	g := NewGomegaWithT(t)

	external := map[string]string{
		"compute:create": "role:member",
		"compute:delete": "role:admin",
	}
	inline := map[string]string{
		"compute:create": "role:admin",
	}

	result := MergePolicies(external, inline)

	g.Expect(result).To(HaveKeyWithValue("compute:create", "role:admin"))
	g.Expect(result).To(HaveKeyWithValue("compute:delete", "role:admin"))
}

func TestMergePolicies_NonCollidingMerged(t *testing.T) {
	g := NewGomegaWithT(t)

	external := map[string]string{"compute:create": "role:member"}
	inline := map[string]string{"identity:list_users": "role:admin"}

	result := MergePolicies(external, inline)

	g.Expect(result).To(HaveLen(2))
	g.Expect(result).To(HaveKeyWithValue("compute:create", "role:member"))
	g.Expect(result).To(HaveKeyWithValue("identity:list_users", "role:admin"))
}

func TestMergePolicies_NilInline(t *testing.T) {
	g := NewGomegaWithT(t)

	external := map[string]string{"compute:create": "role:member"}

	result := MergePolicies(external, nil)

	g.Expect(result).To(HaveLen(1))
	g.Expect(result).To(HaveKeyWithValue("compute:create", "role:member"))
}

func TestMergePolicies_NilExternal(t *testing.T) {
	g := NewGomegaWithT(t)

	inline := map[string]string{"compute:create": "role:admin"}

	result := MergePolicies(nil, inline)

	g.Expect(result).To(HaveLen(1))
	g.Expect(result).To(HaveKeyWithValue("compute:create", "role:admin"))
}

func TestMergePolicies_BothNil(t *testing.T) {
	g := NewGomegaWithT(t)

	result := MergePolicies(nil, nil)

	g.Expect(result).NotTo(BeNil())
	g.Expect(result).To(BeEmpty())
}

func TestMergePolicies_DoesNotMutateInputs(t *testing.T) {
	g := NewGomegaWithT(t)

	external := map[string]string{"compute:create": "role:member"}
	inline := map[string]string{"compute:create": "role:admin"}

	externalCopy := map[string]string{"compute:create": "role:member"}
	inlineCopy := map[string]string{"compute:create": "role:admin"}

	_ = MergePolicies(external, inline)

	g.Expect(external).To(Equal(externalCopy))
	g.Expect(inline).To(Equal(inlineCopy))
}

func TestRenderPolicyYAML_ValidRules(t *testing.T) {
	g := NewGomegaWithT(t)

	rules := map[string]string{
		"compute:create": "role:member",
		"compute:delete": "role:admin",
	}

	result, err := RenderPolicyYAML(rules)

	g.Expect(err).NotTo(HaveOccurred())

	// Verify it is valid YAML.
	var parsed map[string]string
	g.Expect(yaml.Unmarshal([]byte(result), &parsed)).To(Succeed())
	g.Expect(parsed).To(HaveKeyWithValue("compute:create", "role:member"))
	g.Expect(parsed).To(HaveKeyWithValue("compute:delete", "role:admin"))
}

func TestRenderPolicyYAML_EmptyMap(t *testing.T) {
	g := NewGomegaWithT(t)

	result, err := RenderPolicyYAML(map[string]string{})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal("{}\n"))

	// Verify it is valid YAML.
	var parsed map[string]string
	g.Expect(yaml.Unmarshal([]byte(result), &parsed)).To(Succeed())
}

func TestRenderPolicyYAML_SpecialCharacters(t *testing.T) {
	g := NewGomegaWithT(t)

	rules := map[string]string{
		"identity:create_user": "role:admin or (role:member and project_id:%(target.project.id)s)",
		"identity:list_users":  "rule:admin_required",
	}

	result, err := RenderPolicyYAML(rules)

	g.Expect(err).NotTo(HaveOccurred())

	var parsed map[string]string
	g.Expect(yaml.Unmarshal([]byte(result), &parsed)).To(Succeed())
	g.Expect(parsed).To(HaveKeyWithValue("identity:create_user", "role:admin or (role:member and project_id:%(target.project.id)s)"))
}

func TestRenderPolicyYAML_Deterministic(t *testing.T) {
	g := NewGomegaWithT(t)

	rules := map[string]string{
		"z_rule": "value_z",
		"a_rule": "value_a",
		"m_rule": "value_m",
	}

	result1, err := RenderPolicyYAML(rules)
	g.Expect(err).NotTo(HaveOccurred())

	result2, err := RenderPolicyYAML(rules)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(result1).To(Equal(result2))
}

func TestValidatePolicyRules_Valid(t *testing.T) {
	g := NewGomegaWithT(t)

	rules := map[string]string{
		"compute:create": "role:member",
		"compute:delete": "role:admin",
	}
	fldPath := field.NewPath("spec", "policyOverrides", "rules")

	errs := ValidatePolicyRules(rules, fldPath)

	g.Expect(errs).To(BeEmpty())
}

func TestValidatePolicyRules_EmptyKey(t *testing.T) {
	g := NewGomegaWithT(t)

	rules := map[string]string{
		"":               "role:admin",
		"compute:create": "role:member",
	}
	fldPath := field.NewPath("spec", "policyOverrides", "rules")

	errs := ValidatePolicyRules(rules, fldPath)

	g.Expect(errs).To(HaveLen(1))
	g.Expect(errs[0].Type).To(Equal(field.ErrorTypeRequired))
	g.Expect(errs[0].Detail).To(ContainSubstring("key must not be empty"))
}

func TestValidatePolicyRules_EmptyValue(t *testing.T) {
	g := NewGomegaWithT(t)

	rules := map[string]string{
		"compute:create": "",
	}
	fldPath := field.NewPath("spec", "policyOverrides", "rules")

	errs := ValidatePolicyRules(rules, fldPath)

	g.Expect(errs).To(HaveLen(1))
	g.Expect(errs[0].Type).To(Equal(field.ErrorTypeInvalid))
	g.Expect(errs[0].Detail).To(ContainSubstring("value must not be empty"))
}

func TestValidatePolicyRules_MultipleErrors(t *testing.T) {
	g := NewGomegaWithT(t)

	rules := map[string]string{
		"":               "role:admin",
		"compute:create": "",
		"valid:rule":     "role:member",
	}
	fldPath := field.NewPath("spec", "policyOverrides", "rules")

	errs := ValidatePolicyRules(rules, fldPath)

	g.Expect(errs).To(HaveLen(2))
}

func TestValidatePolicyRules_NilMap(t *testing.T) {
	g := NewGomegaWithT(t)

	fldPath := field.NewPath("spec", "policyOverrides", "rules")

	errs := ValidatePolicyRules(nil, fldPath)

	g.Expect(errs).To(BeEmpty())
}

func TestValidatePolicyRules_EmptyMap(t *testing.T) {
	g := NewGomegaWithT(t)

	fldPath := field.NewPath("spec", "policyOverrides", "rules")

	errs := ValidatePolicyRules(map[string]string{}, fldPath)

	g.Expect(errs).To(BeEmpty())
}
