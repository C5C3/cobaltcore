# Review Pattern: Distinguish permission-granted from permission-exercised in security docs

**Review-Area**: documentation
**Detection-Hint**: When documentation claims a permission or capability is absent in a certain context, cross-check the actual workflow/config to see if the permission is declared but merely not used (due to conditional guards). Statements like 'no X usage' are inaccurate if X is granted but skipped.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Review security-related documentation claims about permissions (OIDC tokens, API scopes, IAM roles) against the actual configuration. Verify whether the doc accurately distinguishes between 'permission not granted' vs 'permission granted but steps are conditionally skipped.'

## Why it matters

Inaccurate security documentation misleads security auditors and operators. Claiming a permission isn't used when it's actually granted (just not exercised) misrepresents the attack surface — a malicious or buggy step could still leverage the granted permission.

## Examples from external reviews

### CC-0029 — greptile-apps[bot]
- **Feedback**: However, `id-token: write` **is** present at the job level on `build-base-images`, which runs on PRs (base images are always pushed). The permission is available to all steps in that job, including on PR runs. It's just that the SBOM steps are guarded so they skip — but the permission itself is unconditionally granted.
- **What was missed**: Review security-related documentation claims about permissions (OIDC tokens, API scopes, IAM roles) against the actual configuration. Verify whether the doc accurately distinguishes between 'permission not granted' vs 'permission granted but steps are conditionally skipped.'
- **Fix**: Changed 'No OIDC token requests occur (no `id-token: write` usage)' to 'No OIDC token requests occur — SBOM/attestation steps are skipped so `id-token: write` is not exercised.'
