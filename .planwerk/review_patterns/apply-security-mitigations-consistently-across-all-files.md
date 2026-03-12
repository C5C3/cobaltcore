# Review Pattern: Apply security mitigations consistently across all files

**Review-Area**: security
**Detection-Hint**: When a security pattern or fix is present in one script (e.g., generating secrets in-pod to avoid process listing exposure), search for the same anti-pattern in sibling scripts in the same directory or module. Use grep for similar command structures.
**Severity**: BLOCKING
**Occurrences**: 2

## What to check

When reviewing a file that handles secrets, check whether sibling files in the same bootstrap/deploy workflow use a different (less secure) approach for the same class of operation. A fix applied in write-bootstrap-secrets.sh but not in init-unseal.sh indicates inconsistent application.

## Why it matters

Inconsistent security mitigations create a false sense of safety. If one script avoids exposing secrets in process arguments but another script in the same workflow does not, the overall security posture is only as strong as the weakest link.

## Examples from external reviews

### CC-0009 — greptile-apps[bot]
- **Feedback**: This is the same exposure vector that was addressed in `write-bootstrap-secrets.sh` — where passwords are now generated in-pod via `sh -c "... $(openssl rand ...)"` to avoid cleartext appearing in process arguments. The unseal keys deserve the same treatment.
- **What was missed**: When reviewing a file that handles secrets, check whether sibling files in the same bootstrap/deploy workflow use a different (less secure) approach for the same class of operation. A fix applied in write-bootstrap-secrets.sh but not in init-unseal.sh indicates inconsistent application.
- **Fix**: Added a `kube_exec_stdin` helper in common.sh and used it consistently across both init-unseal.sh and write-bootstrap-secrets.sh for all secret-handling operations.

### CC-0009 — greptile-apps[bot]
- **Feedback**: The four ESO Kubernetes auth roles set `ttl=1h` but omit `token_max_ttl`. Without a max TTL, tokens issued by these roles can be renewed indefinitely by ESO's background renewal loop. By contrast, the AppRole `provisioner` role explicitly caps its tokens with a maximum TTL of 4h.
- **What was missed**: When reviewing auth role definitions (Kubernetes auth, AppRole, etc.), verify that token lifetime bounds like `token_max_ttl` are applied uniformly across roles with similar trust levels. If a stricter role sets `token_max_ttl=4h`, roles at the same or lower trust level should not allow indefinite renewal.
- **Fix**: Added `token_max_ttl=4h` to the ESO Kubernetes auth role loop, matching the AppRole provisioner's ceiling.
