# Review Pattern: Use fixed-string grep when matching literal values

**Review-Area**: validation
**Detection-Hint**: Look for grep commands where the search pattern comes from a variable (e.g., "$pkg", "$name") that contains literal text rather than an intentional regex. Characters like `.`, `+`, `*`, `[`, `]` in package names, filenames, or identifiers will be treated as regex metacharacters, causing false positives.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Any `grep` invocation using a shell variable as the pattern without `-F` (fixed-string) flag. Verify whether the variable content is meant as a literal string or a regex. If literal, `-F` must be present.

## Why it matters

Without `-F`, grep interprets the pattern as a regex. Dots in package names like `libapache2-mod-wsgi-py3` become wildcards matching any character, so `libapache2Xmod-wsgi-py3` would incorrectly match. This creates silent false positives in validation scripts, undermining the test's purpose.

## Examples from external reviews

### CC-0027 — greptile-apps[bot]
- **Feedback**: `grep -qw` matches word boundaries but treats the package name as a regex pattern, where `.` is a metacharacter matching any character. For package names like `libapache2-mod-wsgi-py3`, this means a package like `libapache2Xmod-wsgi-py3` would also match (false positive). Using `-F` (fixed-string mode) would be more robust.
- **What was missed**: Any `grep` invocation using a shell variable as the pattern without `-F` (fixed-string) flag. Verify whether the variable content is meant as a literal string or a regex. If literal, `-F` must be present.
- **Fix**: Changed `grep -qw "$pkg"` to `grep -qFw "$pkg"` to enable fixed-string matching and prevent regex metacharacter interpretation.
