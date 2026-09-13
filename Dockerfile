# Build Stage
FROM --platform=$BUILDPLATFORM golang:alpine AS build-env

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=0.8.0

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN GIT_COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "docker") && \
    BUILD_TIME=$(date -u +"%Y-%m-%dT%H:%M:%SZ") && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -v \
    -ldflags="-s -w -X 'github.com/esrrhs/spp/version.Version=${VERSION}' -X 'github.com/esrrhs/spp/version.GitCommit=${GIT_COMMIT}' -X 'github.com/esrrhs/spp/version.BuildTime=${BUILD_TIME}'" \
    -o spp .

# Final Stage
FROM alpine:latest

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=build-env /app/spp /app/spp

ENTRYPOINT ["/app/spp"]
CMD ["-h"]
