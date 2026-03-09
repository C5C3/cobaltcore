# Pattern: Two-layer OCI annotation approach (static Dockerfile LABEL + CI metadata-action)

**Component**: images/*/Dockerfile + .github/workflows/build-images.yaml
**Category**: configuration
**Applies-When**: Adding a new container image to the build pipeline that should carry OCI Image Spec annotations

## Description

Every container image uses a two-layer OCI annotation approach: (1) static LABEL instructions in the Dockerfile provide four baseline annotations (title, description, licenses=Apache-2.0, vendor=SAP SE) that are always present on locally-built images, and (2) a docker/metadata-action step in the CI workflow generates dynamic labels (created, revision, source, url, version) that supplement the static ones at push time. The metadata-action step is placed immediately before the corresponding build-push-action step, and the build-push-action step wires `labels: ${{ steps.<meta-step-id>.outputs.labels }}`. For service images with an upstream software version, the metadata-action uses `type=raw,value=${{ steps.source-ref.outputs.ref }}` to override the version annotation. In multi-stage Dockerfiles, the LABEL instruction is placed in the runtime stage only (build stage labels are discarded).

## Examples

### `images/python-base/Dockerfile:27-32`

```
# CC-0031: Static OCI Image Spec annotations for local builds.
# CI-generated labels from docker/metadata-action supplement these at push time.
LABEL org.opencontainers.image.title="python-base" \
      org.opencontainers.image.description="Python runtime base image for OpenStack services" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="SAP SE"
```

### `.github/workflows/build-images.yaml:73-84`

```
# CC-0031: Generate OCI annotations for python-base
- name: Generate metadata for python-base
  id: meta-python-base
  uses: docker/metadata-action@c299e40c65443455700f0fdfc63efafe5b349051 # v5
  with:
    images: ghcr.io/${{ steps.meta.outputs.owner }}/python-base
    labels: |
      org.opencontainers.image.title=python-base
      org.opencontainers.image.description=Python runtime base image for OpenStack services
      org.opencontainers.image.licenses=Apache-2.0
      org.opencontainers.image.vendor=SAP SE
```

