# Review Pattern: Verify config-reading scripts handle env var overrides

**Review-Area**: validation
**Detection-Hint**: When inline scripts parse config files, check whether related env vars injected into the container should take precedence
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

For any inline script (Python/shell) that reads config files to extract connection strings or credentials, verify it also consults environment variables injected by the controller, especially when those env vars exist to override placeholder values in the config

## Why it matters

After secret-externalization refactors, config files often contain placeholders while real values flow through env vars. Scripts reading only the config will fail at runtime with DNS/auth errors, breaking Job execution and blocking readiness conditions

## Examples from external reviews

### CC-0080 — berendt
- **Feedback**: The inline Python pre-insert script run before keystone-manage bootstrap parses [database] connection from /etc/keystone/keystone.conf.d/*.conf using stdlib configparser, which has no knowledge of OS_DATABASE__CONNECTION. After CC-0080, the file contains only 'mysql+pymysql://placeholder', so pymysql.connect receives host='placeholder'...
- **What was missed**: For any inline script (Python/shell) that reads config files to extract connection strings or credentials, verify it also consults environment variables injected by the controller, especially when those env vars exist to override placeholder values in the config
- **Fix**: Updated the inline Python script to read OS_DATABASE__CONNECTION from os.environ first and fall back to configparser only when unset
