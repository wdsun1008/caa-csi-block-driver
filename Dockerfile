FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS builder

ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY pkg/ pkg/

RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -ldflags "-X main.version=$VERSION" -o /caa-csi-block-driver ./cmd/

# Multi-platform manifest index: linux/amd64 and linux/s390x are included.
# Update this index deliberately when refreshing the runtime base.
FROM alpine:3.19@sha256:6baf43584bcb78f2e5847d1de515f23499913ac9f12bdf834811a3145eb11ca1
RUN apk add --no-cache e2fsprogs e2fsprogs-extra blkid
COPY --from=builder /caa-csi-block-driver /caa-csi-block-driver
ENTRYPOINT ["/caa-csi-block-driver"]
