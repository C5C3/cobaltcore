# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# ---------- Stage 1: builder ----------
FROM golang:1.26@sha256:6df14f4a4bc9d979a3721f488981e0d1b318006377e473ed23d026796f5f4c0a AS builder

WORKDIR /workspace

# Copy Go workspace and dependency manifests first for layer caching (CC-0017).
COPY go.work go.work.sum ./
COPY internal/common/go.mod internal/common/go.sum ./internal/common/
COPY operators/keystone/go.mod operators/keystone/go.sum ./operators/keystone/
COPY operators/c5c3/go.mod operators/c5c3/go.sum ./operators/c5c3/

RUN go mod download

# Copy source code.
COPY internal/common/ ./internal/common/
COPY operators/keystone/ ./operators/keystone/
COPY operators/c5c3/ ./operators/c5c3/

# Build static binary from operators/keystone/main.go.
WORKDIR /workspace/operators/keystone
RUN CGO_ENABLED=0 GOOS=linux go build -o manager main.go

# ---------- Stage 2: runtime ----------
FROM gcr.io/distroless/static:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240

WORKDIR /

COPY --from=builder /workspace/operators/keystone/manager .

# CC-0017: Static OCI Image Spec annotations for local builds (runtime stage only).
# CI-generated labels from docker/metadata-action supplement these at push time.
LABEL org.opencontainers.image.title="keystone-operator" \
      org.opencontainers.image.description="CobaltCore Keystone Operator for managing OpenStack Identity Service" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="SAP SE"

USER 65532:65532

ENTRYPOINT ["/manager"]
