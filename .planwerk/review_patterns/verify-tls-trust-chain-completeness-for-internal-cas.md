# Review Pattern: Verify TLS trust chain completeness for internal CAs

**Review-Area**: security
**Detection-Hint**: When HTTPS URLs appear alongside a self-signed or internal CA issuer (e.g., cert-manager ClusterIssuer), check that every TLS client in the configuration references the CA certificate. Search for HTTPS endpoints and verify each has a corresponding CA cert path or trust configuration.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

Any config block that connects over HTTPS to a service using a non-public CA must specify the CA certificate for verification. Look for HTTPS URLs in retry_join, upstream, backend, or similar stanzas and confirm a ca_cert / ca_file / leader_ca_cert_file is set. Cross-reference with the certificate issuer to determine if the CA is self-signed or private.

## Why it matters

Without the CA cert, TLS verification fails and the service cannot establish connections. In this case, Raft cluster formation was completely broken — follower nodes could never join the leader, making the entire HA deployment non-functional.

## Examples from external reviews

### CC-0009 — greptile-apps[bot]
- **Feedback**: The three `retry_join` stanzas use `https://` URLs but include no `leader_ca_cert_file` (or `leader_tls_servername` + skip-verify). The TLS certificate is issued by `selfsigned-cluster-issuer` — a ClusterIssuer that produces a self-signed CA not present in the pod's OS trust store.
- **What was missed**: Any config block that connects over HTTPS to a service using a non-public CA must specify the CA certificate for verification. Look for HTTPS URLs in retry_join, upstream, backend, or similar stanzas and confirm a ca_cert / ca_file / leader_ca_cert_file is set. Cross-reference with the certificate issuer to determine if the CA is self-signed or private.
- **Fix**: Added `leader_ca_cert_file = "/openbao/tls/ca.crt"` to each of the three `retry_join` stanzas, referencing the CA already mounted from the `openbao-tls` Secret.
