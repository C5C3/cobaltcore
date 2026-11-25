// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

import { execFileSync } from 'node:child_process'

const branch = process.env.GITHUB_REF_NAME ?? execFileSync('git', ['branch', '--show-current'], { encoding: 'utf8' }).trim()
const mode = process.env.RELEASE_MODE
const series = /^(release|promote)\/(0|[1-9]\d*)\.(0|[1-9]\d*)$/.exec(branch)
if (!series || !['candidate', 'stable'].includes(mode) || (series[1] === 'promote' && mode !== 'stable')) {
  throw new Error('Select a release/X.Y branch and candidate or stable. A promote/X.Y branch may only promote stable.')
}

const bootstrap = !execFileSync('git', ['tag', '--list', 'v0.1.0'], { encoding: 'utf8' }).trim()
export const github = {
  assets: ['assets/*.tgz', 'assets/IMAGE_DIGESTS.json', 'assets/SHA256SUMS', 'assets/RELEASE_NOTES.md'],
  successComment: false,
  failComment: false,
  releasedLabels: false,
  labels: false,
}
// The initial changelog exceeds GitHub's 125,000-character release body limit.
const releaseUrl = process.env.GITHUB_SERVER_URL + '/' + process.env.GITHUB_REPOSITORY + '/releases'
github.releaseBodyTemplate = '<% if (nextRelease.notes.length <= 120000) { %><%= nextRelease.notes %><% } else { %>' +
  '[Full release notes](' + releaseUrl + '/download/<%= nextRelease.gitTag %>/RELEASE_NOTES.md)<% } %>'
if (process.env.CANDIDATE_TAG) {
  github.releaseBodyTemplate = 'Promoted from [' + process.env.CANDIDATE_TAG + '](' +
    process.env.GITHUB_SERVER_URL + '/' + process.env.GITHUB_REPOSITORY + '/releases/tag/' +
    process.env.CANDIDATE_TAG + ').\n\n' + github.releaseBodyTemplate
}

export default {
  branches: mode === 'candidate' ? ['main', { name: branch, prerelease: 'rc', channel: 'rc' }] : [branch],
  tagFormat: 'v${version}',
  plugins: [
    ['@semantic-release/commit-analyzer', bootstrap ? { releaseRules: [{ breaking: true, release: 'minor' }] } : {}],
    '@semantic-release/release-notes-generator',
    {
      verifyRelease(_, { nextRelease }) {
        if (process.env.GITHUB_SHA && nextRelease.gitHead !== process.env.GITHUB_SHA) {
          throw new Error('The computed release commit differs from the selected workflow commit')
        }
        const base = nextRelease.version.split('-')[0]
        if (!base.startsWith(series[2] + '.' + series[3] + '.') || (bootstrap && base !== '0.1.0')) {
          throw new Error('Computed ' + nextRelease.version + ' outside the selected release series ' + branch)
        }
        if (process.env.EXPECTED_VERSION && nextRelease.version !== process.env.EXPECTED_VERSION) {
          throw new Error('Release changed after artifact preparation: ' + nextRelease.version)
        }
      },
    },
    ...(process.env.RELEASE_PREVIEW === 'true' ? [] : [['@semantic-release/github', github]]),
  ],
}
