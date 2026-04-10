# Pattern: Workflow-to-script env-var bridge for CI script invocation

**Component**: .github/workflows/build-images.yaml, hack/ci-*.sh
**Category**: service-structure
**Applies-When**: Invoking a hack/ci-*.sh script from a GitHub Actions workflow step where the script needs GitHub Actions context values

## Description

When a workflow step invokes a hack/ci-*.sh script, all GitHub Actions context values (${{ }}) are passed via the step's env: block, never interpolated into the run: command. The script reads these via ${VAR:?message} for required vars and ${VAR:-default} for optional vars. This extends the existing 'CI script extraction with env-var interface' pattern to the build-images workflow specifically, covering IMAGE, DIGEST_DIR, TAGS, INSPECT_TAG for ci-merge-manifest.sh and SERVICE_NAME, SERVICE_VERSION, INSTALL_SPEC, VENV_BUILDER_IMAGE, RELEASE for ci-run-unit-tests.sh.

## Examples

### `.github/workflows/build-images.yaml:237-242`

```
      - name: Create and push python-base manifest
        id: merge-python-base
        env:
          IMAGE: ghcr.io/${{ steps.meta.outputs.owner }}/python-base
          DIGEST_DIR: /tmp/digests/python-base
          TAGS: ghcr.io/${{ steps.meta.outputs.owner }}/python-base:latest ghcr.io/${{ steps.meta.outputs.owner }}/python-base:${{ github.sha }}
          INSPECT_TAG: ghcr.io/${{ steps.meta.outputs.owner }}/python-base:${{ github.sha }}
        run: hack/ci-merge-manifest.sh
```

### `.github/workflows/build-images.yaml:878-889`

```
      - name: Run tests
        env:
          SERVICE_NAME: ${{ matrix.service }}
          SERVICE_VERSION: ${{ steps.checkout-source.outputs.source-ref }}
          INSTALL_SPEC: ${{ steps.pip-extras.outputs.install_spec }}
          VENV_BUILDER_IMAGE: ${{ needs.merge-base-images.outputs.venv-builder-image }}
          RELEASE: ${{ matrix.release }}
          OS_TEST_DBAPI_ADMIN_CONNECTION: "sqlite://;mysql+pymysql://root:openstack@127.0.0.1/;postgresql+psycopg2://openstack:openstack@127.0.0.1/openstack"
        run: hack/ci-run-unit-tests.sh
```

