# Review Pattern: Database cursor not closed before connection close

**Review-Area**: error-handling
**Detection-Hint**: In database access code, check that cursors are explicitly closed (or managed via context managers) before the connection is closed. Look for conn.close() calls without a preceding cur.close().
**Severity**: WARNING
**Occurrences**: 1

## What to check

Are all cursor objects explicitly closed before their parent connection is closed? Are cleanup paths (including error/exception paths) closing resources in the correct inner-to-outer order?

## Why it matters

Relying on connection close to implicitly release cursors can leak server-side resources (prepared statements, locks) depending on the database driver, and makes cleanup ordering fragile in error paths.

## Examples from external reviews

### CC-0064 — berendt
- **Feedback**: Added explicit cur.close() before conn.close() in the Python script.
- **What was missed**: Are all cursor objects explicitly closed before their parent connection is closed? Are cleanup paths (including error/exception paths) closing resources in the correct inner-to-outer order?
- **Fix**: Added an explicit cur.close() call before conn.close() to ensure proper resource cleanup ordering.
