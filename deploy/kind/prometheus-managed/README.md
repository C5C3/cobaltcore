# Prometheus with a platform-owned namespace

Use this composition after `kind/base` or another owner has created Namespace
`monitoring`. It has the same HelmRelease/dashboard as [the standalone
entry point](../prometheus/), but omits the Namespace so it cannot overwrite
platform metadata. The built-in kind installer and metal-stack overlay use it.
The original `deploy/kind/prometheus` remains compatible for standalone callers.

Stage the real dashboard before rendering:

```sh
make stage-prometheus-dashboard
kubectl kustomize deploy/kind/prometheus-managed
```

The source, cert-manager and Flux prerequisites of the standalone addon still
apply. The external `EXTERNAL_OVERLAY/prometheus/` contract is unchanged.
