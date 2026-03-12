# Review Pattern: Verify all pod specs share required volume mounts

**Review-Area**: architecture
**Detection-Hint**: When a PR introduces multiple pod specs (Jobs, Deployments, StatefulSets) that run the same application binary, compare each spec's volumes/volumeMounts side-by-side. If one spec mounts a config file that the binary requires (e.g. keystone.conf), every other spec running that binary must also mount it.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

For every container spec in the PR that runs an application command (e.g. keystone-manage db_sync, keystone-manage bootstrap), verify it has the same configuration volumes and mounts as the Deployment container running the same image. Diff the volume/volumeMount sections across all pod-creating resources.

## Why it matters

A Job or pod that runs an application binary without its required config file will fail immediately and permanently. In this case both the db_sync and bootstrap Jobs ran keystone-manage without mounting keystone.conf, causing oslo.config RequiredOptError failures that permanently blocked the entire reconcile chain.

## Examples from external reviews

### CC-0013 — greptile-apps[bot]
- **Feedback**: `buildDBSyncJob` creates a Job with `keystone-manage db_sync` but no volume or environment variable provides a database connection. `keystone-manage db_sync` reads `[database] connection` from `keystone.conf` via oslo.config; without it, the command exits immediately with `oslo_config.cfg.RequiredOptError`.
- **What was missed**: For every container spec in the PR that runs an application command (e.g. keystone-manage db_sync, keystone-manage bootstrap), verify it has the same configuration volumes and mounts as the Deployment container running the same image. Diff the volume/volumeMount sections across all pod-creating resources.
- **Fix**: Added volumes and volumeMounts entries to both the db_sync and bootstrap Job specs to mount the config ConfigMap at /etc/keystone/keystone.conf.d/, matching the pattern already used by the Deployment. Updated buildDBSyncJob and buildBootstrapJob to accept configMapName as a parameter.
