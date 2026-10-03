# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# openstack-hypervisor-operator (hvo), compiled from a pinned commit of its
# upstream main branch with the patches under patches/ applied. The pin is a
# main commit and not a tag: main carries the required --agent-namespaces
# flag, the offboarding taint and parallel migrations in an Eviction, and no
# tag (v1.2.3 is the latest) has them. Upstream publishes a Helm chart for
# every main commit but no image under a fixed tag for it, only the moving
# 'latest'; this build fills that gap. The chart of a main commit renders the
# image as <repository>:<appVersion> with appVersion sha-<full commit>, and
# the image is published under that tag, so the chart runs it with only the
# repository overridden. Release-independent: the image has no key in
# releases/*/source-refs.yaml, and the pinned commit, not a release, names
# its tags. Like the other images it is rebuilt by every build-images run on
# main, which moves sha-<commit>, upstream-<commit> and latest to that build.

# A commit of upstream main and the single pin of this image, bumped by the
# Renovate customManager for this file in renovate.json.
# hack/ci-resolve-hvo-commit.sh is the only parser of this line. A move on
# which a patch no longer applies fails the build at the 'git apply' step
# below; re-cut the patch against the new commit. Once upstream carries the
# change, drop the patch together with its checks in the build step below,
# and once no patch remains, the COPY patches/ and 'git apply' steps too:
# both fail without a patch to apply.
ARG HVO_COMMIT=a2baf3f4002e39ff93f105fae9594b357ba55b21

# ---------- Stage 1: build ----------
FROM golang:1.27@sha256:3680233e3204827fbdc66088528ae6d4b3d034f51d03a99d454f6de034888244 AS build

# An ARG declared before the first FROM is global; each stage re-declares it
# bare to read its value.
ARG HVO_COMMIT

# Source checkout by the exact commit, so no mutable ref decides what is
# compiled. The github_token secret is optional and handled as in
# images/ovn/Dockerfile: a basic-auth header scoped to github.com, set
# through git's environment config in this one RUN and in no layer.
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
RUN --mount=type=secret,id=github_token \
    export GIT_TERMINAL_PROMPT=0 && \
    if [ -s /run/secrets/github_token ]; then \
        auth="$(printf 'x-access-token:%s' "$(cat /run/secrets/github_token)" | base64 -w0)" && \
        export GIT_CONFIG_COUNT=1 \
            GIT_CONFIG_KEY_0="http.https://github.com/.extraheader" \
            GIT_CONFIG_VALUE_0="AUTHORIZATION: basic ${auth}"; \
    fi && \
    git init /src && \
    git -C /src remote add origin https://github.com/cobaltcore-dev/openstack-hypervisor-operator.git && \
    git -C /src fetch --depth 1 origin "${HVO_COMMIT}" && \
    git -C /src checkout --detach FETCH_HEAD

# The modules get a layer of their own before the patches, as in
# operators/Dockerfile: CI's type=gha build cache keeps layers but no cache
# mounts, so a change to patches/ or to a step below reuses the downloaded
# modules instead of fetching the whole module graph again.
WORKDIR /src
ENV GOTOOLCHAIN=local CGO_ENABLED=0
RUN go mod download

COPY patches/ /patches/
RUN for p in /patches/*.patch; do \
        git apply --index "${p}" || { echo "patch does not apply: ${p}" >&2; exit 1; }; \
    done

# The LiveMigrateOpts grep and the go test below are patch 0001's checks. No
# code outside the tests may still name gophercloud's servers.LiveMigrateOpts:
# its BlockMigration is a *bool and cannot carry "auto", so the check catches
# a live migration that a pin move adds or reshapes, and a re-cut that lost
# the controller.go hunk. The grep matches the bare type name, not one way of
# writing it, so a literal, a 'var', new(), a pointer and an aliased import
# all fail the build. Only a '//' outside a double-quoted string before the
# name on its line lets the line pass, so a comment passes while a '/' in a
# string or a division does not. The test the patch brings proves its body;
# 'go test -run' exits 0 with "[no tests to run]" when the test is missing, so
# the grep for its PASS line is what fails a patch that lost its test hunk.
# The second go test and its two greps are the checks of patches 0002 and
# 0003, one test each. Its -run expression does not match the package's
# Ginkgo entry point (TestControllers in suite_test.go), so no envtest binary
# is needed, and the space after each test name keeps a subtest's PASS line
# from matching. The ldflags are upstream's (Makefile, the build/manager
# target) with the version set to the tag the image is published under.
# Upstream's 'generate' step is skipped: the generated files are committed.
RUN --mount=type=cache,target=/root/.cache/go-build \
    if grep -rnE --include='*.go' --exclude='*_test.go' '^([^/"]|/[^/]|"([^"\\]|\\.)*")*\bLiveMigrateOpts\b' .; then \
        echo "a live migration still sends servers.LiveMigrateOpts; extend patch 0001" >&2; exit 1; \
    fi && \
    go test -count=1 -v -run '^TestLiveMigrateAutoBody$' ./internal/controller/eviction/ \
        | tee /tmp/patch-test.log && \
    grep -q -- '--- PASS: TestLiveMigrateAutoBody' /tmp/patch-test.log && \
    go test -count=1 -v \
        -run '^(TestHypervisorCreatedWithDefaultHighAvailability|TestTraitsInSyncSetsTraitsUpdated)$' \
        ./internal/controller/ | tee /tmp/patch-test-controller.log && \
    grep -q -- '--- PASS: TestHypervisorCreatedWithDefaultHighAvailability ' /tmp/patch-test-controller.log && \
    grep -q -- '--- PASS: TestTraitsInSyncSetsTraitsUpdated ' /tmp/patch-test-controller.log && \
    go build -trimpath \
        -ldflags "-s -w \
          -X github.com/sapcc/go-api-declarations/bininfo.binName=manager \
          -X github.com/sapcc/go-api-declarations/bininfo.version=sha-${HVO_COMMIT} \
          -X github.com/sapcc/go-api-declarations/bininfo.commit=${HVO_COMMIT}" \
        -o /out/manager ./cmd

# ---------- Stage 2: runtime ----------
FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3

ARG HVO_COMMIT
COPY --from=build --link /out/manager /usr/bin/manager

# DEVIATION from the original build-pipeline design:
# This image does not derive from python-base and creates no 'openstack'
# user. The operator is a static Go binary, so it runs on the distroless
# base the CobaltCore operator images use (operators/Dockerfile), as that
# base's nonroot user, uid 65532. Upstream's own image is Alpine with uid
# 4200; its chart only sets runAsNonRoot and no command, which any non-root
# user and the ENTRYPOINT below satisfy.
LABEL org.opencontainers.image.title="openstack-hypervisor-operator" \
      org.opencontainers.image.description="openstack-hypervisor-operator built from a pinned upstream main commit with CobaltCore patches" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="SAP SE" \
      io.c5c3.upstream-commit="${HVO_COMMIT}"

USER 65532:65532
ENTRYPOINT ["/usr/bin/manager"]
