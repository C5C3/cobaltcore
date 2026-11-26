// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

import { appendFileSync, writeFileSync } from 'node:fs'
import semanticRelease from 'semantic-release'

const result = await semanticRelease({ dryRun: true })
if (result) {
  const { nextRelease, lastRelease } = result
  if (nextRelease.gitHead !== process.env.GITHUB_SHA) throw new Error('Release branch moved')
  writeFileSync('release-plan.json', JSON.stringify({ nextRelease, lastRelease }) + '\n')
  appendFileSync(process.env.GITHUB_OUTPUT, 'version=' + nextRelease.version + '\n')
  appendFileSync(process.env.GITHUB_STEP_SUMMARY, 'Planned **' + nextRelease.gitTag + '** at `' + nextRelease.gitHead + '`.\n')
}
