# Review Pattern: Prevent shell injection in sh -c command strings

**Review-Area**: security
**Detection-Hint**: Search for `sh -c` or `bash -c` where variables are interpolated into the command string via `${var}` or `$var`. Even single-quoted interpolation like `'${var}'` inside a double-quoted `sh -c "..."` is vulnerable if the variable contains single quotes.
**Severity**: WARNING
**Occurrences**: 2

## What to check

Variables embedded in `sh -c` command strings should be passed via environment variables (`env VAR=val sh -c '... $VAR ...'`) or positional arguments (`sh -c '... "$1"' _ "$var"`) rather than interpolated into the command string. This eliminates the entire class of shell injection bugs.

## Why it matters

String interpolation into `sh -c` commands can cause syntax errors or shell token injection if the variable contains quotes or shell metacharacters. Even if current callers use safe values, future callers may not, and the failure mode ranges from confusing errors to command injection.

## Examples from external reviews

### CC-0009 — greptile-apps[bot]
- **Feedback**: `kv_path` is embedded in the inner shell command as a single-quoted string: `sh -c "bao kv put '${kv_path}' ${put_args}"`. If a future caller passes a `kv_path` containing a single quote, the shell command will be misparsed.
- **What was missed**: Variables embedded in `sh -c` command strings should be passed via environment variables (`env VAR=val sh -c '... $VAR ...'`) or positional arguments (`sh -c '... "$1"' _ "$var"`) rather than interpolated into the command string. This eliminates the entire class of shell injection bugs.
- **Fix**: Refactored to pass `kv_path` via the `BAO_KV_PATH` environment variable instead of interpolating it into the `sh -c` command string.

### CC-0009 — greptile-apps[bot]
- **Feedback**: If any future caller passes a `val` that contains a single quote (e.g., `description=it's-a-service`), the single quote will prematurely terminate the single-quoted segment in the inner shell, causing a parse error — or worse, allowing unintended shell token injection.
- **What was missed**: Any pattern where a variable is wrapped in single quotes and then interpolated into a double-quoted string executed by `sh -c` or `eval`. Specifically: (1) Is the value escaped for the quoting context it's placed in? (2) Can a single quote in the value break out of the quoting boundary? (3) Does this create a shell injection vector?
- **Fix**: Escape single quotes in the value before wrapping: `local escaped_val="${val//\'/\'\\\'\'}"; put_args+=" ${key}='${escaped_val}'"` — replacing each `'` with `'\''` which safely exits and re-enters single quoting around a literal quote.
