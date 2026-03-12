# Review Pattern: Never skip TLS verification when CA cert is available

**Review-Area**: security
**Detection-Hint**: Search for `SKIP_VERIFY`, `--insecure`, `--no-check-certificate`, `-k` (curl), `verify=False`, `NODE_TLS_REJECT_UNAUTHORIZED=0` or equivalent TLS bypass flags. Then check whether the corresponding CA certificate or trust chain is already mounted/available in the environment.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

When self-signed or internal CA certs are used, the code should reference the CA cert explicitly (e.g., `VAULT_CACERT`) rather than disabling verification entirely. Verify that all exec wrappers and environment injections propagate the CA path, not a skip-verify flag.

## Why it matters

Disabling TLS verification removes server identity checks, allowing silent MITM attacks. When the CA cert is already available in the environment (mounted from the same Secret), using it costs nothing and provides actual authentication.

## Examples from external reviews

### CC-0009 — greptile-apps[bot]
- **Feedback**: Setting `VAULT_SKIP_VERIFY=true` disables certificate verification entirely for all `bao` commands in every bootstrap script. While TLS is still used for transport encryption, the server's identity is never checked — a man-in-the-middle with a different certificate would succeed silently.
- **What was missed**: When self-signed or internal CA certs are used, the code should reference the CA cert explicitly (e.g., `VAULT_CACERT`) rather than disabling verification entirely. Verify that all exec wrappers and environment injections propagate the CA path, not a skip-verify flag.
- **Fix**: Replaced `VAULT_SKIP_VERIFY=true` with `VAULT_CACERT=/openbao/tls/ca.crt` across all exec wrappers in common.sh, init-unseal.sh, and write-bootstrap-secrets.sh.
