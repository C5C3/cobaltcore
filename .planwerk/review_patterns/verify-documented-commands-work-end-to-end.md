# Review Pattern: Verify documented commands work end-to-end

**Review-Area**: documentation
**Detection-Hint**: When docs contain shell commands (especially multi-step build sequences), mentally execute them in order and cross-reference any referenced names (image tags, paths, variables) against the actual source files (Dockerfiles, configs).
**Severity**: BLOCKING
**Occurrences**: 4

## What to check

Check that image tags used in `docker build -t` commands match the `FROM` directives in downstream Dockerfiles. More generally, verify that documented commands are self-consistent and would succeed if copy-pasted in sequence.

## Why it matters

Users following the docs verbatim get a build failure. The docs become a source of confusion rather than help, and undermine trust in the project's documentation quality.

## Examples from external reviews

### CC-0006 — greptile-apps[bot]
- **Feedback**: Build commands produce wrong tag names — `docker build images/venv-builder` would fail as written. The commands tag the image as `c5c3/python-base:3.12-noble`, but `images/venv-builder/Dockerfile` begins with `FROM python-base`.
- **What was missed**: Check that image tags used in `docker build -t` commands match the `FROM` directives in downstream Dockerfiles. More generally, verify that documented commands are self-consistent and would succeed if copy-pasted in sequence.
- **Fix**: Changed `docker build images/python-base -t c5c3/python-base:3.12-noble` to `docker build images/python-base -t python-base` so the tag matches the `FROM python-base` directive in downstream Dockerfiles.

### CC-0006 — greptile-apps[bot]
- **Feedback**: This always exits 0. `echo $?` captures and prints the exit code of `which gcc` as text, but since `echo` itself exits 0, `sh -c` exits 0, and `docker run` exits 0. A developer who wraps this in an `if` statement or CI script will always see success — even if gcc IS present in the image.
- **What was missed**: Any documented verification command (especially negative checks like 'verify X is NOT present') must actually exit non-zero when the check fails. Watch for `cmd; echo $?` which prints but discards the exit code, and `cmd || true` which swallows it.
- **Fix**: Changed from `sh -c 'which gcc; echo $?'` to `which gcc && echo 'FAIL' || echo 'PASS'` which correctly propagates the exit code.

### CC-0029 — greptile-apps[bot]
- **Feedback**: The `--type` flag in `cosign verify-attestation` expects a predicate type URI or a recognized shorthand (e.g., `slsaprovenance`, `slsaprovenance1`, `link`, `vuln`). `cyclonedx` is not a standard shorthand in cosign.
- **What was missed**: Confirm that every flag, argument, and value in documented example commands is actually accepted by the tool. For cosign/sigstore/gh tooling, verify that predicate type shorthands exist and that the recommended verification path matches how attestations are actually stored.
- **Fix**: Replaced the invalid `cosign verify-attestation --type cyclonedx` example with the recommended `gh attestation` tooling using the correct CycloneDX predicate type URI `https://cyclonedx.org/bom`.

### CC-0052 — sourcery-ai[bot]
- **Feedback**: The usage text suggests `make test-race [OPERATOR=keystone]`, but the target actually iterates over `$(OPERATORS)` and ignores `OPERATOR` passed on the command line. This inconsistency can mislead users who expect to run race tests for a single operator.
- **What was missed**: Does the code actually reference every parameter mentioned in its usage documentation? If a comment says `make test-race [OPERATOR=keystone]`, does the target body use `$(OPERATOR)` to filter the loop?
- **Fix**: Either plumbed the `OPERATOR` variable into the target to filter the loop, or updated the usage comment to accurately reflect the current behavior.
