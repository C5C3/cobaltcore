# Review Pattern: Verify explicit resource cleanup on opened connections

**Review-Area**: error-handling
**Detection-Hint**: When reviewing code that opens a connection (HTTP, DB, file handle), trace the code path to ensure .close() or equivalent cleanup is called before exit, even when the process will terminate shortly after.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Every connection/resource opened in a script or function has a corresponding explicit close call, especially when extracting inline code into standalone scripts intended to be independently testable.

## Why it matters

Implicit cleanup on process exit is unreliable in long-running contexts and violates explicit resource management patterns. When code moves from inline to standalone scripts, previously harmless implicit cleanup becomes a real risk.

## Examples from external reviews

### CC-0073 — berendt
- **Feedback**: Both fernet_rotate.sh and credential_rotate.sh read the HTTP response but never call conn.close(). While http.client.HTTPSConnection will be cleaned up when the Python process exits, the project pattern requires explicit resource cleanup ordering.
- **What was missed**: Every connection/resource opened in a script or function has a corresponding explicit close call, especially when extracting inline code into standalone scripts intended to be independently testable.
- **Fix**: Added conn.close() before the success print statement in both fernet_rotate.sh and credential_rotate.sh.
