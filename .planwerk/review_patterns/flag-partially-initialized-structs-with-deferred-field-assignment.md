# Review Pattern: Flag partially initialized structs with deferred field assignment

**Review-Area**: testing
**Detection-Hint**: When a struct (especially an object key or identifier) is declared early with some fields empty and filled in many lines later, check whether code between declaration and completion could accidentally use the incomplete struct.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Look for struct declarations where required fields (like Name in a NamespacedName) are left zero-valued and assigned far from the declaration site. Verify the struct is not used in the gap, and prefer moving the declaration to the point where all required fields are known.

## Why it matters

An empty Name in a NamespacedName will never match any real object. If a helper call is inserted in the gap between declaration and assignment, it silently operates on nothing — causing confusing test failures or false passes that are hard to diagnose.

## Examples from external reviews

### CC-0014 — greptile-apps[bot]
- **Feedback**: `key` is initialized with only `Namespace` set at line 345, and `Name` is patched in at line 386 after the CR is created. While the current code is correct, this pattern is fragile: any helper call inserted between lines 345 and 386 that passes `key` would silently operate on a namespaced name with an empty `Name` string.
- **What was missed**: Look for struct declarations where required fields (like Name in a NamespacedName) are left zero-valued and assigned far from the declaration site. Verify the struct is not used in the gap, and prefer moving the declaration to the point where all required fields are known.
- **Fix**: Move `key` declaration to immediately after the CR is created: `ks := integrationBrownfieldKeystone("test-keystone", ns.Name); c.Create(ctx, ks); key := types.NamespacedName{Name: ks.Name, Namespace: ns.Name}`.
