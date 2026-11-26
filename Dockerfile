# syntax=docker/dockerfile:1
ARG GO_VERSION=1.27.4-alpine
ARG PG_TAG=19-alpine

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/ad-postgres-backup ./cmd/ad-postgres-backup

FROM postgres:${PG_TAG}

LABEL org.opencontainers.image.source="https://github.com/phantomwar/ad-postgres-backup" \
      org.opencontainers.image.description="Drop-in PostgreSQL 16-19 GFS local, S3 offsite, and age-encrypted backup & restore-test image" \
      org.opencontainers.image.licenses="MIT"

RUN apk add --no-cache ca-certificates tzdata su-exec \
    && mkdir -p /backups /hooks \
    && chown -R postgres:postgres /backups

COPY --from=builder /out/ad-postgres-backup /usr/local/bin/ad-postgres-backup
COPY --chmod=0755 scripts/backup-shim.sh /backup.sh

ENV BACKUP_DIR=/backups \
    BACKUP_SUFFIX=.sql.gz \
    BACKUP_LATEST_TYPE=symlink \
    BACKUP_KEEP_MINS=1440 \
    BACKUP_KEEP_DAYS=7 \
    BACKUP_KEEP_WEEKS=4 \
    BACKUP_KEEP_MONTHS=6 \
    BACKUP_CHECKSUM=true \
    BACKUP_MAX_STALE_HOURS=36 \
    DISK_MAX_USAGE_PCT=92 \
    SCHEDULE=@daily \
    HEALTHCHECK_PORT=8080 \
    VALIDATE_ON_START=true \
    S3_FORCE_PATH_STYLE=true \
    S3_REQUIRE_ENCRYPTION=true \
    S3_RETENTION_DAYS=30

VOLUME ["/backups"]
EXPOSE 8080

HEALTHCHECK --interval=1m --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/ad-postgres-backup", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/ad-postgres-backup"]
CMD ["daemon"]
