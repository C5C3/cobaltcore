# Pattern: Per-service Tempest config directory with tempest.conf + include/exclude test lists

**Component**: tests/tempest/*/
**Category**: testing
**Applies-When**: Adding Tempest API tests for a new OpenStack service/release (e.g., tests/tempest/keystone-2026-1/)

## Description

Each service/release combination tested by Tempest has a directory under tests/tempest/<service>-<release-slug>/ (e.g., keystone-2025-2, keystone-2026-1) containing three files: tempest.conf (oslo.config format with service endpoint, auth credentials via ${ENV_VAR} placeholder, and service_available flags), include-tests.txt (regex patterns for tests to run), and exclude-tests.txt (regex patterns for tests to skip due to missing dependencies). The tempest.conf uses in-cluster DNS for the service endpoint (e.g., keystone-tempest-2025-2-api.openstack.svc:5000) which is rewritten to localhost in CI via sed. Both hack/run-tempest.sh and ci.yaml dynamically check for the existence of this directory before running Tempest.

## Examples

### `tests/tempest/keystone-2025-2/tempest.conf:9-32`

```
[DEFAULT]
log_dir = /tmp/tempest-logs
log_file = tempest.log

[identity]
uri_v3 = http://keystone-basic-api.openstack.svc:5000/v3

[auth]
use_dynamic_credentials = false
admin_username = admin
admin_password = ${KEYSTONE_ADMIN_PASSWORD}
admin_project_name = admin
admin_domain_name = Default

[identity-feature-enabled]
api_v3 = true

[service_available]
identity = true
compute = false
network = false
volume = false
image = false
object-storage = false
```

### `tests/tempest/keystone-2025-2/include-tests.txt:11-15`

```
# Core Tempest identity API tests
tempest.api.identity

# Keystone-specific plugin tests
keystone_tempest_plugin.tests
```

