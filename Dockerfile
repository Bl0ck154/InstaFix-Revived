# syntax=docker/dockerfile:1

ARG BUILDPLATFORM=linux/amd64
FROM --platform=$BUILDPLATFORM golang:1.27.3 as app-builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
COPY BingSiteAuth.xml ./
COPY handlers/ ./handlers/
COPY utils/ ./utils/
COPY observability/ ./observability/
COPY views/ ./views/
COPY video/instagram7-reel/out/instagram7-test-reel.mp4 ./video/instagram7-reel/out/instagram7-test-reel.mp4
COPY video/instagram7-reel/out/instagram7-test-reel-poster.webp ./video/instagram7-reel/out/instagram7-test-reel-poster.webp

ARG TARGETARCH
ARG COMMIT_SHA=dev
ARG BUILD_VERSION=dev
ARG BUILD_TIME=unknown

# CGO=0 keeps the application binary portable. Build identity is compiled in
# so /healthz can prove exactly which image is running.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build \
    -tags netgo,osusergo \
    -ldflags "-s -w -X main.buildCommit=${COMMIT_SHA} -X main.buildVersion=${BUILD_VERSION} -X main.buildTime=${BUILD_TIME}" \
    -o /app/instafix .

# Oversized Telegram reels need one narrow fallback that remuxes Instagram's
# separate DASH video+audio tracks. Alpine keeps the ffmpeg runtime relatively
# small while the Go service itself remains an unprivileged static binary.
FROM alpine:3.22
RUN apk add --no-cache ca-certificates ffmpeg \
    && chmod 1777 /tmp
COPY --from=app-builder /app/instafix /instafix

WORKDIR /tmp
USER 65532:65532

EXPOSE 3000

ENV GOMEMLIMIT=384MiB
ENV GOGC=50
ENV TMPDIR=/tmp

ENTRYPOINT ["/instafix"]
