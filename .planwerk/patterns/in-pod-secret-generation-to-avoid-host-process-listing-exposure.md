# Pattern: In-pod secret generation to avoid host process listing exposure

**Component**: deploy/openbao/bootstrap/
**Category**: error-handling
**Applies-When**: Writing bootstrap scripts that generate or pass secret values via kubectl exec — generate secrets inside the pod using sh -c with command substitution, never as host-side command arguments

## Description

When a bootstrap script needs to generate or write secret values (passwords, tokens, keys), the secret must be generated inside the target pod via sh -c with $(openssl rand -base64 32) or equivalent. Passing generated secrets as kubectl exec positional arguments exposes them in /proc/<pid>/cmdline on the host. The pattern uses a marker value (@generate) in the calling code, which is replaced with an in-pod command substitution in the sh -c string. Non-secret values (e.g., username=keystone) can be passed directly. The kv_path is passed via the BAO_KV_PATH env var rather than interpolated into the sh -c command string to prevent shell injection.

## Examples

### `deploy/openbao/bootstrap/write-bootstrap-secrets.sh:53-54`

```
    if [[ "${val}" == "${GENERATED_PASSWORD}" ]]; then
      put_args+=" ${key}=\"\$(openssl rand -base64 32)\""
```

### `deploy/openbao/bootstrap/write-bootstrap-secrets.sh:66-69`

```
  kubectl exec -n "$NAMESPACE" openbao-0 -- \
    env BAO_ADDR="${BAO_ADDR}" BAO_TOKEN="${BAO_TOKEN}" VAULT_CACERT="${VAULT_CACERT}" \
    BAO_KV_PATH="${kv_path}" \
    sh -c "bao kv put \"\${BAO_KV_PATH}\" ${put_args}"
```
