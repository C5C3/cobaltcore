# Review Pattern: Documentation using present tense for planned features

**Review-Area**: documentation
**Detection-Hint**: When docs describe CI triggers, integrations, or automation, verify whether the described mechanism actually exists. Look for present-tense claims ('runs on', 'triggers when') that reference pipelines, workflows, or hooks not yet implemented.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Cross-reference any documentation section describing CI behavior or automated triggers against the actual CI config files (.github/workflows/, .gitlab-ci.yml, etc.). If the described trigger or pipeline doesn't exist in the repo, the docs must clearly mark it as planned/future.

## Why it matters

Present-tense documentation for non-existent CI integration misleads contributors into believing tests run automatically, so they skip manual verification — or waste time searching for a pipeline that doesn't exist.

## Examples from external reviews

### CC-0047 — berendt
- **Feedback**: W-006 was fixed by changing the CI Trigger Policy heading to '(Planned)' with future tense and an explicit note that CI integration is deferred.
- **What was missed**: Cross-reference any documentation section describing CI behavior or automated triggers against the actual CI config files (.github/workflows/, .gitlab-ci.yml, etc.). If the described trigger or pipeline doesn't exist in the repo, the docs must clearly mark it as planned/future.
- **Fix**: Changed CI Trigger Policy heading to '(Planned)' with future tense language and an explicit note that CI integration is deferred.
