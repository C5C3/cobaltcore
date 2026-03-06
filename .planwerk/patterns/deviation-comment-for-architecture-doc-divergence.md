# Pattern: DEVIATION comment for architecture doc divergence

**Component**: images/*/Dockerfile
**Category**: configuration
**Applies-When**: Implementing a Dockerfile (or other infrastructure file) that intentionally deviates from the architecture documentation in architecture/docs/

## Description

When the implementation deliberately deviates from the architecture documentation, a multi-line `# DEVIATION from architecture/docs/<path>:` comment is added at the point of deviation. The comment (1) names the specific architecture doc file, (2) explains what the architecture doc specifies, (3) explains what the implementation does instead, and (4) provides rationale. Cross-references between related DEVIATION comments point readers to the primary explanation. This pattern appears in 2 Dockerfiles for the generic user vs per-service user decision.

## Examples

### `images/python-base/Dockerfile:19-23`

```
# DEVIATION from architecture/docs/08-container-images/01-build-pipeline.md:
# The architecture doc's Keystone Dockerfile example creates a per-service user
# (e.g., groupadd keystone / useradd keystone). We use a generic 'openstack'
# user/group (UID/GID 42424) shared by all service images to reduce complexity
# and image layers. Each service image inherits this user via USER openstack.
```

### `images/keystone/Dockerfile:24-26`

```
# DEVIATION from architecture/docs/08-container-images/01-build-pipeline.md:
# Uses the generic 'openstack' user (UID/GID 42424) from python-base instead
# of a per-service user. See python-base/Dockerfile for rationale.
```

