# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# ---------- Stage 1: builder ----------
FROM golang:1.26@sha256:d184d9be4c13614e28498d632eeaaac704d662f18ad357e1df74a44424236cea AS builder

WORKDIR /workspace

# Copy Go workspace and dependency manifests first for layer caching (CC-0017).
COPY go.work go.work.sum ./
COPY internal/common/go.mod internal/common/go.sum ./internal/common/
COPY operators/keystone/go.mod operators/keystone/go.sum ./operators/keystone/
COPY operators/c5c3/go.mod operators/c5c3/go.sum ./operators/c5c3/

RUN go mod download

# Copy source code. c5c3 depends on keystone via replace ../keystone (CC-0110),
# so the keystone module source must be present in the build context too.
COPY internal/common/ ./internal/common/
COPY operators/keystone/ ./operators/keystone/
COPY operators/c5c3/ ./operators/c5c3/

# Build static binary from operators/c5c3/main.go.
WORKDIR /workspace/operators/c5c3
RUN CGO_ENABLED=0 GOOS=linux go build -o manager main.go

# ---------- Stage 2: runtime ----------
FROM gcr.io/distroless/static:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240

WORKDIR /

COPY --from=builder /workspace/operators/c5c3/manager .

# CC-0017: Static OCI Image Spec annotations for local builds (runtime stage only).
# CI-generated labels from docker/metadata-action supplement these at push time.
LABEL org.opencontainers.image.title="c5c3-operator" \
      org.opencontainers.image.description="CobaltCore C5C3 Operator for managing OpenStack ControlPlane resources" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="SAP SE"

USER 65532:65532

ENTRYPOINT ["/manager"]
