# Pattern: Poem comments on simulator files per reviewer convention

**Component**: testutil/simulators
**Category**: naming
**Applies-When**: External reviewer (berendt) explicitly requests decorative poem comments on simulator source files

## Description

The project maintainer (berendt) has requested poem comments at the top of simulator .go files (before the package declaration). This is a reviewer-driven convention specific to this package. The poem describes the simulator's purpose in a lyrical format. Not all files have poems yet — they are added incrementally per reviewer request.

## Examples

### `internal/common/testutil/simulators/certificate.go:1`

```go
// A certificate is forged in trust's quiet flame,
// its issuer signs the secret with a name.
// Through TLS handshakes, data travels sealed,
// no plaintext whisper on the wire revealed.
//
// In envtest realms where cert-manager sleeps,
// this simulator wakes and vigil keeps.
// It stamps the status, sets the condition True,
// so reconcile may pass its rendezvous.
//
// No ACME dance, no challenge left unsolved—
// the ready state, by code alone, resolved.
// Sleep well, dear cert, your chain of trust is whole;
// a simulated flame can warm a testing soul.
```

