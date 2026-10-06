# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# kvm-node-agent (kna), compiled from a pinned commit of its upstream main
# branch with the patches under patches/ applied. The pin is a main commit:
# upstream publishes a Helm chart for every main commit but no image for it
# (ghcr.io/cobaltcore-dev/kvm-node-agent:sha-<commit> answers 404), and a fix
# kna needs outside SAP ships here as a patch while it is proposed upstream.
# The chart of a main commit renders the image as <repository>:<appVersion>
# with appVersion sha-<full commit>, and the image is published under that
# tag, so the chart runs it with only the repository overridden.
# Release-independent: the image has no key in releases/*/source-refs.yaml,
# and the pinned commit, not a release, names its tags. Like the other images
# it is rebuilt by every build-images run on main, which moves sha-<commit>,
# upstream-<commit> and latest to that build.

# A commit of upstream main and the single pin of this image, bumped by the
# Renovate customManager for this file in renovate.json.
# hack/ci-resolve-kna-commit.sh is the only parser of this line. A move on
# which a patch no longer applies fails the build at the 'git apply' step
# below; re-cut the patch against the new commit. Once upstream carries the
# change, drop the patch together with its checks in the build step below,
# and once no patch remains, the COPY patches/ and 'git apply' steps too:
# both fail without a patch to apply.
ARG KNA_COMMIT=1e4e4b8e7050bbfa2d5a990aa21b849ba7b51700

# ---------- Stage 1: build ----------
FROM golang:1.27@sha256:3680233e3204827fbdc66088528ae6d4b3d034f51d03a99d454f6de034888244 AS build

# An ARG declared before the first FROM is global; each stage re-declares it
# bare to read its value.
ARG KNA_COMMIT

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
    git -C /src remote add origin https://github.com/cobaltcore-dev/kvm-node-agent.git && \
    git -C /src fetch --depth 1 origin "${KNA_COMMIT}" && \
    git -C /src checkout --detach FETCH_HEAD

# The modules get a layer of their own before the patches, as in
# operators/Dockerfile: CI's type=gha build cache keeps layers but no cache
# mounts, so a change to patches/ or to a step below reuses the downloaded
# modules instead of fetching the whole module graph again.
WORKDIR /src
ENV GOTOOLCHAIN=local CGO_ENABLED=0
RUN go mod download

# The index is refreshed first: 'git apply --index' compares each file's
# stat data with the index and never its content, and a checkout layer that
# CI restores from the build cache has new inodes and change times. Without
# the refresh such a build fails every patch with "does not match index".
COPY patches/ /patches/
RUN git update-index -q --refresh && \
    for p in /patches/*.patch; do \
        git apply --index "${p}" || { echo "patch does not apply: ${p}" >&2; exit 1; }; \
    done

# The two test runs below are patch 0001's checks. The test binary is
# compiled once, with -trimpath as in the go build below, so that build
# reuses the packages compiled for it. A test binary exits 0 with
# "testing: warning: no tests to run" when the test is missing, so the grep
# for each PASS line is what fails a patch that lost its test hunk; the space
# after the test name keeps a subtest's PASS line from matching. The first
# run is root, as is every RUN here. The second runs as uid 65534, because a
# root process may give a file any group and
# TestUpdateTLSCertificateKeyGroupNotPermitted skips as root: a skipped test
# prints '--- SKIP', so the second grep fails if that run was root. Once
# upstream carries the change, both checks go with the patch; a later patch
# adds its file under patches/ and its check here.
# The ldflags are upstream's (Makefile, the build/manager target) without the
# build date, with the version set to the tag the image is published under.
# Upstream's 'generate' step is skipped: the generated files are committed.
RUN --mount=type=cache,target=/root/.cache/go-build \
    go test -c -trimpath -o /tmp/certificates.test ./internal/certificates/ && \
    env -C internal/certificates /tmp/certificates.test -test.v \
        -test.run '^TestUpdateTLSCertificateKeyMode$' | tee /tmp/patch-test.log && \
    grep -q -- '--- PASS: TestUpdateTLSCertificateKeyMode ' /tmp/patch-test.log && \
    setpriv --reuid=65534 --regid=65534 --clear-groups \
        env -C internal/certificates /tmp/certificates.test -test.v \
        -test.run '^TestUpdateTLSCertificateKeyGroupNotPermitted$' | tee /tmp/patch-test-nobody.log && \
    grep -q -- '--- PASS: TestUpdateTLSCertificateKeyGroupNotPermitted ' /tmp/patch-test-nobody.log && \
    go build -trimpath \
        -ldflags "-s -w \
          -X github.com/sapcc/go-api-declarations/bininfo.binName=manager \
          -X github.com/sapcc/go-api-declarations/bininfo.version=sha-${KNA_COMMIT} \
          -X github.com/sapcc/go-api-declarations/bininfo.commit=${KNA_COMMIT}" \
        -o /out/manager ./cmd

# ---------- Stage 2: runtime ----------
FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3

ARG KNA_COMMIT
COPY --from=build --link /out/manager /usr/bin/manager

# DEVIATION from the original build-pipeline design:
# This image does not derive from python-base and creates no 'openstack'
# user. The agent is a static Go binary, so it runs on the distroless base
# the CobaltCore operator images use (operators/Dockerfile), but as root
# instead of that base's nonroot user: kna authenticates to the host's
# system bus and starts units there, and a Debian host's dbus-daemon drops
# a uid it cannot resolve (#1167). Upstream's own image is Alpine with
# USER 42438:42438, a user that exists on SAP's hosts only.
LABEL org.opencontainers.image.title="kvm-node-agent" \
      org.opencontainers.image.description="kvm-node-agent built from a pinned upstream main commit with CobaltCore patches" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="SAP SE" \
      io.c5c3.upstream-commit="${KNA_COMMIT}"

# hadolint ignore=DL3002
USER 0:0
ENTRYPOINT ["/usr/bin/manager"]
