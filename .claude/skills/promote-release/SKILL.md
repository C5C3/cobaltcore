---
name: promote-release
description: Promote a validated CobaltCore release candidate through semantic-release.
---

# Promote a CobaltCore candidate

Fetch the distribution tags. Identify the highest numeric RC for the target
version, confirm its GitHub prerelease is published, and compare its commit
with the approved validation evidence.

When the maintainer requests stable publication, dispatch CI at that exact
commit. If `release/X.Y` still points there, use the series branch:

```bash
gh workflow run ci.yaml --ref release/0.1 -f distribution=stable
```

If the series branch has advanced, use a `promote/X.Y` branch at the approved
SHA. Create and publish that branch only with authorization. Verify any
existing promotion branch before using it. The workflow rejects a candidate
superseded by a higher RC for the same version.

Follow the run and return the stable GitHub Release link. Verify its image
digest manifest matches the RC and its stable chart renders the stable image
tag. Semantic-release creates the stable Git tag and publishes the chart
archives, image manifest and checksums.
