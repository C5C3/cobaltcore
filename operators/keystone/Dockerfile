# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# ---------- Stage 1: builder ----------
FROM golang:1.25@sha256:c0a2bd0756d92462a0d449124b039100ce447ebf69dc6c80a6d877503b36935e AS builder

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
FROM gcr.io/distroless/static:nonroot@sha256:e3f945647ffb95b5839c07038d64f9811adf17308b9121d8a2b87b6a22a80a39

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
