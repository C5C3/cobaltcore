# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Parameterized operator image: one Dockerfile builds every operator binary in
# the Go workspace. Select the operator via --build-arg OPERATOR=<op> (e.g.
# keystone, c5c3); adding a new operator to go.work needs no Dockerfile edit —
# only the module-manifest COPY lines below grow by one.

# ---------- Stage 1: builder ----------
FROM golang:1.26@sha256:45a5f7a810238aabcbad211d70b9ae082022d96f7c7259e94041ad1b933575ac AS builder

# ARG does not survive FROM; re-declare it in each stage that uses it.
ARG OPERATOR

WORKDIR /workspace

# Copy Go workspace and dependency manifests first for layer caching.
COPY go.work go.work.sum ./
COPY internal/common/go.mod internal/common/go.sum ./internal/common/
COPY operators/keystone/go.mod operators/keystone/go.sum ./operators/keystone/
COPY operators/c5c3/go.mod operators/c5c3/go.sum ./operators/c5c3/
COPY operators/horizon/go.mod operators/horizon/go.sum ./operators/horizon/
COPY operators/glance/go.mod operators/glance/go.sum ./operators/glance/
COPY operators/placement/go.mod operators/placement/go.sum ./operators/placement/
COPY operators/barbican/go.mod operators/barbican/go.sum ./operators/barbican/
COPY operators/ovn/go.mod operators/ovn/go.sum ./operators/ovn/

RUN go mod download

# Copy source code. Every workspace module is copied regardless of OPERATOR:
# the workspace resolves cross-module replace directives (e.g. c5c3 depends on
# keystone via replace ../keystone), so all module sources must be present.
COPY internal/common/ ./internal/common/
COPY operators/keystone/ ./operators/keystone/
COPY operators/c5c3/ ./operators/c5c3/
COPY operators/horizon/ ./operators/horizon/
COPY operators/glance/ ./operators/glance/
COPY operators/placement/ ./operators/placement/
COPY operators/barbican/ ./operators/barbican/
COPY operators/ovn/ ./operators/ovn/

# Build the selected operator's static binary from its main.go.
WORKDIR /workspace/operators/${OPERATOR}
RUN CGO_ENABLED=0 GOOS=linux go build -o manager main.go

# ---------- Stage 2: runtime ----------
FROM gcr.io/distroless/static:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7

ARG OPERATOR

WORKDIR /

COPY --from=builder /workspace/operators/${OPERATOR}/manager .

# Static OCI Image Spec annotations for local builds (runtime stage only).
# CI-generated labels from docker/metadata-action supplement these at push time.
LABEL org.opencontainers.image.title="${OPERATOR}-operator" \
      org.opencontainers.image.description="CobaltCore ${OPERATOR} operator" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="SAP SE"

USER 65532:65532

ENTRYPOINT ["/manager"]
