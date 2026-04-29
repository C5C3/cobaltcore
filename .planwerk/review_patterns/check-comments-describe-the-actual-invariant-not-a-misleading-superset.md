# Review Pattern: Check comments describe the actual invariant, not a misleading superset

**Review-Area**: documentation
**Detection-Hint**: When reviewing comments on policy/config files that describe what is 'protected' or 'forbidden', map the comment's claim against the glob/rule semantics and confirm it matches the actual invariant rather than a broader intuition.
**Severity**: WARNING
**Occurrences**: 2

## What to check

For comments on ACL rules, regex/glob policies, or access-control declarations, verify the comment accurately describes which paths the rule does and does not cover. Watch for comments that imply protection of sibling paths the glob doesn't actually cover, or that conflate 'leaf unwritable' with 'subtree unwritable'.

## Why it matters

Misleading comments on security-sensitive policies create false confidence: reviewers and future maintainers may rely on the comment instead of the rule, leading to incorrect assumptions about what is actually protected.

## Examples from external reviews

### CC-0093 — berendt
- **Feedback**: The HCL policy comment in deploy/openbao/policies/push-keystone-keys.hcl:29-45 was rewritten to accurately describe the invariant — the `db` leaf itself remains unwritable, and sibling paths `db/fernet-keys`/`db/credential-keys` would match the `+` glob but are independent KV-v2 keys that cannot overwrite the `db` secret.
- **What was missed**: For comments on ACL rules, regex/glob policies, or access-control declarations, verify the comment accurately describes which paths the rule does and does not cover. Watch for comments that imply protection of sibling paths the glob doesn't actually cover, or that conflate 'leaf unwritable' with 'subtree unwritable'.
- **Fix**: Rewrote the HCL policy comment to state the true invariant (db leaf protection) and clarify that sibling paths matching the `+` glob are independent KV-v2 keys, not overwrites of `db`.

### CC-0096 — gndrmnn
- **Feedback**: [same comment flagged at keystone_types.go, config/crd/bases YAML, and helm/keystone-operator/crds YAML]
- **What was missed**: Whether comment changes in API types are reflected (or need to be reflected) in all generated CRD manifests so reviewers don't have to flag the same text three times.
- **Fix**: Edited the godoc once and regenerated both CRD YAMLs so all three locations were addressed together.
