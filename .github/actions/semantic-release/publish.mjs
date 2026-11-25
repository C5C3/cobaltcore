// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

import { execFileSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import { readFileSync, readdirSync } from 'node:fs'
import semanticRelease from 'semantic-release'
import { publish } from '@semantic-release/github'
import { github } from '../../../release.config.mjs'

const { nextRelease, lastRelease } = JSON.parse(readFileSync('release-plan.json', 'utf8'))
const { gitTag, gitHead, version } = nextRelease
const env = process.env
if (version !== env.EXPECTED_VERSION || gitTag !== 'v' + version || gitHead !== env.GITHUB_SHA) {
  throw new Error('The saved release plan differs from the artifacts or the checkout')
}

const git = (...args) => execFileSync('git', args, { encoding: 'utf8' }).trim()
const gh = (...args) => execFileSync('gh', args, { encoding: 'utf8' })
if (!git('tag', '--list', gitTag)) {
  const result = await semanticRelease()
  if (result?.nextRelease.gitTag !== gitTag) throw new Error('Release branch moved. Dispatch a new release.')
} else {
  // Core writes the tag before GitHub uploads assets. Resume the original
  // run using its saved notes and immutable version after an interrupted upload.
  if (git('rev-parse', gitTag + '^{commit}') !== gitHead) throw new Error('Release tag moved')
  const prerelease = env.RELEASE_MODE === 'candidate'
  const pages = JSON.parse(gh('api', '--paginate', '--slurp', 'repos/{owner}/{repo}/releases?per_page=100'))
  const release = pages.flat().find(item => item.tag_name === gitTag)
  if (!release) {
    await publish(github, {
      cwd: process.cwd(), env, logger: console, nextRelease, lastRelease,
      branch: { name: env.GITHUB_REF_NAME, type: prerelease ? 'prerelease' : 'release', main: !prerelease },
      options: { repositoryUrl: env.GITHUB_SERVER_URL + '/' + env.GITHUB_REPOSITORY },
    })
  } else {
    if (release.prerelease !== prerelease) throw new Error('Existing release has a different channel')
    const files = new Map(readdirSync('assets').map(name => [
      name, 'sha256:' + createHash('sha256').update(readFileSync('assets/' + name)).digest('hex'),
    ]))
    for (const asset of release.assets) {
      if (files.get(asset.name) !== asset.digest) throw new Error('Existing asset differs: ' + asset.name)
      files.delete(asset.name)
    }
    if (!release.draft && files.size) throw new Error('Published release is missing assets')
    if (release.draft) {
      for (const name of files.keys()) gh('release', 'upload', gitTag, 'assets/' + name)
      gh('release', 'edit', gitTag, '--draft=false', '--latest=' + !prerelease)
    }
  }
}

const published = JSON.parse(gh('release', 'view', gitTag, '--json', 'assets,isDraft,isPrerelease'))
const localFiles = readdirSync('assets')
if (published.isDraft || published.isPrerelease !== (env.RELEASE_MODE === 'candidate') ||
    published.assets.length !== localFiles.length) throw new Error('Published release has unexpected assets or visibility')
for (const name of localFiles) {
  const digest = 'sha256:' + createHash('sha256').update(readFileSync('assets/' + name)).digest('hex')
  if (published.assets.find(asset => asset.name === name)?.digest !== digest) {
    throw new Error('Published asset differs: ' + name)
  }
}
