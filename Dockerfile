ARG GO_VERSION=1.26
ARG XX_VERSION=1.9.0

FROM --platform=$BUILDPLATFORM tonistiigi/xx:${XX_VERSION} AS xx
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder

ARG TARGETPLATFORM
ARG TARGETOS
ARG TARGETARCH
ARG VERSION

# Copy the build utilities.
COPY --from=xx / /

WORKDIR /workspace

# copy modules manifests
COPY go.mod go.mod
COPY go.sum go.sum

# cache modules
RUN go mod download

# copy source code
COPY cmd/flux-mirror/ cmd/flux-mirror/
COPY internal/ internal/
COPY api/ api/

# build
ENV CGO_ENABLED=0
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w -X main.VERSION=${VERSION}" \
    -a -o /usr/local/bin/flux-mirror ./cmd/flux-mirror/

FROM alpine:3.23

RUN apk --no-cache add ca-certificates \
  && update-ca-certificates

COPY --from=builder /usr/local/bin/flux-mirror /usr/local/bin/

USER 65534:65534

ENTRYPOINT [ "flux-mirror" ]
