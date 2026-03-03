# Pattern: Poem header comment in integration test files

**Component**: integration tests
**Category**: naming
**Applies-When**: Adding or modifying integration test files

## Description

All integration test files have a multi-line poem comment before the //go:build integration tag. The poem describes the package's functionality in verse form (typically 13-14 lines in rhyming couplets). This pattern was introduced by external reviewer request and is now consistently applied across all 9 integration test files.

## Examples

### `internal/common/secrets/secrets_integration_test.go:1`

```go
// A secret rests where etcd's shadows dwell,
// its payload locked in base64's quiet spell.
// The ExternalSecret fetches what is sealed,
// and only when it's Ready stands revealed.
//
// A PushSecret reverses fortune's tide,
// it sends the local truth to vaults outside.
// Idempotent, it knocks but once per name,
// and owner refs ensure the cleanup game.
//
// With envtest's stage we prove each guarded door,
// that ready means ready — nothing less, nothing more.
// So test by test the contract holds its ground;
// in integration's forge, the truth is found.
```

### `internal/common/config/configmap_integration_test.go:1`

```go
// A ConfigMap is born from data's gentle hand,
// its keys and values carefully planned.
// A hash is woven through its given name,
// so changed content never looks the same.
//
// Immutable it stands, steadfast and true,
// no careless patch may alter what it knew.
// Idempotent the call — create it twice,
// the cluster answers once, precise and nice.
//
// With owner references the bond is set,
// a parent's seal the child shall not forget.
// When garbage collection sweeps the floor,
// the orphaned map shall trouble us no more.
//
// So here we test, with envtest's gentle stage,
// each verse a case upon this integration page.
```

