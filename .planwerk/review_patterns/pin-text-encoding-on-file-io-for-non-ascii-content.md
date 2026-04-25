# Review Pattern: Pin text encoding on file I/O for non-ASCII content

**Review-Area**: validation
**Detection-Hint**: Search for `read_text(`/`write_text(`/`open(` calls in scripts that handle non-ASCII characters (em-dashes, smart quotes, accented chars) and confirm `encoding="utf-8"` is passed. Without it, behavior depends on the runtime locale.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Python file I/O calls in generators, formatters, and check scripts should explicitly specify `encoding="utf-8"` whenever content may contain non-ASCII characters, so round-tripping is deterministic across CI runners and developer machines.

## Why it matters

Locale-dependent encoding causes drift checks and generators to produce different bytes on different machines, leading to spurious diffs, flaky CI, or silent corruption of characters like em-dashes in fixtures.

## Examples from external reviews

### CC-0094 — berendt
- **Feedback**: I-001 was resolved by threading `encoding="utf-8"` through both `target.read_text()` and `target.write_text()` calls so em-dash round-tripping is locale-independent.
- **What was missed**: Python file I/O calls in generators, formatters, and check scripts should explicitly specify `encoding="utf-8"` whenever content may contain non-ASCII characters, so round-tripping is deterministic across CI runners and developer machines.
- **Fix**: Threaded `encoding="utf-8"` through both `target.read_text()` and `target.write_text()` calls in `_generate.py`.
