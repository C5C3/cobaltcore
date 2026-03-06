# Pattern: SPDX Apache-2.0 header on infrastructure files

**Component**: images/*, releases/*, scripts/*
**Category**: configuration
**Applies-When**: Adding any new Dockerfile, YAML config, or shell script to the repository (extends the existing GitHub Actions SPDX pattern to all infrastructure files)

## Description

All infrastructure files (Dockerfiles, YAML configs, shell scripts) start with the SPDX license header using the same copyright text as .github/workflows/ci.yaml. Dockerfiles and YAML files use `# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company` on line 1, blank comment on line 2, `# SPDX-License-Identifier: Apache-2.0` on line 3. Shell scripts place the shebang on line 1 and SPDX header on lines 2-4. This extends the existing github-actions-workflow-spdx-header-and-structure-convention pattern beyond workflows to all repo files.

## Examples

### `images/python-base/Dockerfile:1-3`

```
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
```

### `scripts/apply-constraint-overrides.sh:1-4`

```
#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
```

