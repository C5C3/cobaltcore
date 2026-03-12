# Review Pattern: Ensure credentials have bounded lifetimes

**Review-Area**: security
**Detection-Hint**: Search for TTL values set to 0 or empty strings in secret/auth configuration (e.g., secret_id_ttl=0, token_ttl=0). Compare credential lifetimes within the same system for consistency — if tokens expire but the credentials used to obtain them do not, flag the inconsistency.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Any credential (AppRole secret ID, API key, service account token) configured with no expiration (TTL=0) while other credentials in the same authentication chain have bounded lifetimes. A never-expiring secret ID undermines time-bounded token TTLs.

## Why it matters

A leaked credential with no expiration can be used indefinitely to obtain fresh tokens, making the blast radius of a compromise unbounded. This contradicts least-privilege principles already established by bounded token TTLs in the same system.

## Examples from external reviews

### CC-0009 — greptile-apps[bot]
- **Feedback**: The `secret_id_ttl` for the `provisioner` role is set to `0`, meaning issued secret IDs have no expiry. If a secret ID is leaked, it can be used indefinitely to obtain fresh tokens — bounded only by the 4h token max TTL per authentication, not by any overall window.
- **What was missed**: Any credential (AppRole secret ID, API key, service account token) configured with no expiration (TTL=0) while other credentials in the same authentication chain have bounded lifetimes. A never-expiring secret ID undermines time-bounded token TTLs.
- **Fix**: Changed `secret_id_ttl=0` to `secret_id_ttl=8760h` (1 year) with a comment explaining the rationale for periodic rotation.
