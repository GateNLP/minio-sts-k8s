FROM --platform=$BUILDPLATFORM tonistiigi/xx AS xx

FROM --platform=$BUILDPLATFORM golang:1.24 AS builder

WORKDIR /tmp/go-build

COPY --from=xx / /
ARG TARGETPLATFORM
ENV CGO_ENABLED=0

COPY go.mod go.sum ./
RUN xx-go mod download

COPY pkg ./pkg/
COPY cmd ./cmd/

FROM --platform=$BUILDPLATFORM builder AS build-webhook
RUN xx-go build ./cmd/sts-webhook && xx-verify ./sts-webhook

FROM --platform=$BUILDPLATFORM builder AS build-sidecar
RUN xx-go build ./cmd/sts-sidecar && xx-verify ./sts-sidecar

FROM gcr.io/distroless/static-debian12 AS webhook
COPY --from=build-webhook /tmp/go-build/sts-webhook /sts-webhook
ENTRYPOINT ["/sts-webhook"]

FROM gcr.io/distroless/static-debian12 AS sidecar
COPY --from=build-sidecar /tmp/go-build/sts-sidecar /sts-sidecar
ENTRYPOINT ["/sts-sidecar"]

