# ControlPlane operator stage

Build with `kubectl kustomize deploy/bases/controlplane-operator`. Owns only the
c5c3 operator HelmRelease from its [canonical release directory](../../flux-system/releases/c5c3-operator/).
It installs no ControlPlane instance or static database/cache.

Requires the infrastructure/service operators and K-ORC APIs. K-ORC is a Flux
Kustomization, so a HelmRelease `dependsOn` cannot express that dependency; gate
this stage in the deployment repository. Complete the chosen bootstrap/secret
prerequisites before admitting the service stack.

Patch the chart's supported controller values with a targeted HelmRelease patch.
Customize resulting services through their ControlPlane/service CRs, not operator
resource requests. Submit a ControlPlane before database tenant onboarding, then
wait for final readiness afterward to avoid a circular bootstrap gate.

Keep published Flux-managed operators separate from the external development/CI
operator mode of `make deploy-infra`. Removal and pruning uninstall the operator;
it does not replace the documented service/data teardown lifecycle.
