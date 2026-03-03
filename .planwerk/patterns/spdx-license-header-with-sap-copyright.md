# Pattern: SPDX license header with SAP copyright

**Component**: .github/workflows, docs
**Category**: configuration
**Applies-When**: Creating any new file in the repository that requires license attribution (workflows, documentation, source code)

## Description

All files include an SPDX-compliant license header with 'Copyright 2026 SAP SE or an SAP affiliate company' and Apache-2.0 identifier. Format adapts to file type: YAML/shell uses '#' comments, Markdown uses HTML comments '<!-- -->'.

## Examples

### `.github/workflows/ci.yaml:1`

```
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
---
```

### `docs/ci-workflow.md:1`

```
<!--
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
-->
```

