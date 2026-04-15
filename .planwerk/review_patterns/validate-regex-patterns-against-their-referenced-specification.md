# Review Pattern: Validate regex patterns against their referenced specification

**Review-Area**: validation
**Detection-Hint**: When a regex validates identifiers that follow an external standard (PEP 508, semver, RFC, etc.), compare the regex character class against the specification's allowed characters. Look for character classes like `[a-z0-9_]` and ask: does the spec also allow hyphens, dots, or other characters?
**Severity**: WARNING
**Occurrences**: 2

## What to check

That validation regexes for standard-defined identifiers (package names, extras, versions) accept the full set of characters permitted by the relevant specification, not just the characters that happen to appear in current test data.

## Why it matters

An overly restrictive validation silently rejects valid inputs. The test passes today because existing data doesn't exercise the gap, but it becomes a false-negative trap that blocks legitimate future additions with no clear error pointing to the regex as the cause.

## Examples from external reviews

### CC-0027 — greptile-apps[bot]
- **Feedback**: The validation pattern `^[a-z][a-z0-9_]*$` does not allow hyphens. PEP 508 defines extra names using the same normalized identifier rules as package names, which permit hyphens (e.g. `oslo-messaging`, `oslo-policy`). The current three extras (`ldap`, `memcache_pool`, `oauth1`) all match, but any future OpenStack service extra that uses a hyphen would be incorrectly rejected by this test.
- **What was missed**: That validation regexes for standard-defined identifiers (package names, extras, versions) accept the full set of characters permitted by the relevant specification, not just the characters that happen to appear in current test data.
- **Fix**: Changed the regex from `'^[a-z][a-z0-9_]*$'` to `'^[a-z][a-z0-9_-]*$'` and updated the corresponding error message to reflect the allowed character set.

### CC-0077 — sourcery-ai[bot]
- **Feedback**: `retain` is used directly in `len(candidates) <= retain` and as the start index in `for i := retain; i < len(candidates); i++`. If a negative value is ever passed (e.g., via a misconfigured CR), `candidates[i]` will panic at runtime. Consider normalizing `retain` (e.g., clamp to 0) or returning a validation error when `retain < 0`.
- **What was missed**: Any integer parameter that controls loop bounds or slice indexing must be validated or clamped before use. Specifically: (1) is the parameter used in a slice index like `candidates[retain:]` or a loop like `for i := retain; ...`? (2) Can the parameter ever be negative at runtime (e.g., from user input, CR fields, or deserialization defaults)? If yes to both, a guard clause is required.
- **Fix**: A guard clause was added to clamp negative retain values to 0 at function entry, and a test with retain=-5 was added to verify the clamping behavior.
