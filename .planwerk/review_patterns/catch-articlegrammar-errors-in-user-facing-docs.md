# Review Pattern: Catch article/grammar errors in user-facing docs

**Review-Area**: documentation
**Detection-Hint**: Read prose changes in docs/*.md aloud or scan for 'a' followed by a vowel-sound word ('a hourly', 'a HTTP', 'a SQL') and 'an' followed by a consonant-sound word.
**Severity**: WARNING
**Occurrences**: 1

## What to check

User-facing documentation should use correct articles ('an hourly', not 'a hourly') and other basic grammar; reviewers should proofread prose changes, not just code blocks.

## Why it matters

Reference docs are read by external users; small grammar errors erode trust and signal that docs aren't carefully maintained.

## Examples from external reviews

### CC-0096 — sourcery-ai[bot]
- **Feedback**: Use "an hourly" instead of "a hourly" for correct grammar. Since "hourly" begins with a vowel sound, the article should be "an".
- **What was missed**: User-facing documentation should use correct articles ('an hourly', not 'a hourly') and other basic grammar; reviewers should proofread prose changes, not just code blocks.
- **Fix**: Change 'populates a hourly schedule' to 'populates an hourly schedule' in docs/reference/keystone-crd.md.
