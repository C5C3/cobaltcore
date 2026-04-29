# Review Pattern: Verify shell command portability in docs

**Review-Area**: documentation
**Detection-Hint**: When runbooks include shell command substitutions, check whether flags are GNU-specific (e.g. `date -d`, `readlink -f`, `sed -i ''`) and would break on macOS/BSD where many operators run their terminals.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Any `$(...)` or backtick command substitution in operator-facing docs should use POSIX-portable flags, or the doc should explicitly state the required platform / provide both GNU and BSD variants. Pay special attention to `date`, `sed`, `readlink`, `stat`, `xargs`.

## Why it matters

Operators following a runbook from a Mac workstation will hit `illegal option` errors at the worst possible time (e.g., during incident response). A doc that fails on half the team's laptops is worse than a doc that asks them to substitute a value manually.

## Examples from external reviews

### CC-0096 — berendt
- **Feedback**: The `$(date -u -d '7 days ago' +%FT%TZ)` is evaluated by the operator's local shell before being passed to `kubectl exec`. `date -d` is a GNU coreutils extension; it does not exist on macOS/BSD. Operators following this runbook from a Mac workstation will get `date: illegal option -- d`.
- **What was missed**: Any `$(...)` or backtick command substitution in operator-facing docs should use POSIX-portable flags, or the doc should explicitly state the required platform / provide both GNU and BSD variants. Pay special attention to `date`, `sed`, `readlink`, `stat`, `xargs`.
- **Fix**: Replaced the GNU-only `$(date -u -d '7 days ago' +%FT%TZ)` substitution with a literal ISO-8601 timestamp and a comment instructing the operator to substitute a date 7 days before today.
