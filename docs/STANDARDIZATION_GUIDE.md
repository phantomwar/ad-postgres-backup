# PostgreSQL 19 Standardization & Safe Upgrade Guide

This guide documents standard practices for deploying `ad-postgres-backup` and upgrading PostgreSQL 16/17/18 stacks safely to **PostgreSQL 19 (`postgres:19-alpine`)**.

---

## 1. Critical PostgreSQL 18+ `PGDATA` Change

Starting with PostgreSQL 18+ official Docker images (`postgres:18-alpine`, `postgres:19-alpine`), the upstream image changed its default `VOLUME` and `PGDATA`:

* **PG <= 17:** `PGDATA=/var/lib/postgresql/data` with volume mounted at `/var/lib/postgresql/data`.
* **PG 18 / 19:** `PGDATA=/var/lib/postgresql/19/docker` (version-specific subdirectory) with volume mounted at **`/var/lib/postgresql`**.

Never point a brand-new `postgres:19-alpine` container directly at an existing PG 16/17/18 data volume. Major PostgreSQL versions have incompatible binary page/WAL catalog formats and must be upgraded cleanly via `pg_dump` -> `pg_restore` (or `pg_upgrade`) into a dedicated volume.

---

## 2. Recommended Target Compose Pattern

```yaml
services:
  postgres:
    image: postgres:19-alpine
    restart: unless-stopped
    environment:
      POSTGRES_USER: ${POSTGRES_USER}
      POSTGRES_PASSWORD: ${DB_PASS}
      POSTGRES_DB: ${POSTGRES_DB}
      TZ: UTC
    volumes:
      - app_pg19_data:/var/lib/postgresql
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U ${POSTGRES_USER} -d ${POSTGRES_DB}"]
      interval: 10s
      timeout: 5s
      retries: 5
      start_period: 15s

  db-backup:
    image: ghcr.io/phantomwar/ad-postgres-backup:19-alpine
    restart: unless-stopped
    depends_on:
      postgres:
        condition: service_healthy
    volumes:
      - ./backups:/backups
    environment:
      POSTGRES_HOST: postgres
      POSTGRES_USER: ${POSTGRES_USER}
      POSTGRES_PASSWORD: ${DB_PASS}
      POSTGRES_DB: ${POSTGRES_DB}
      POSTGRES_EXTRA_OPTS: "-Fc"
      BACKUP_SUFFIX: ".dump"
      BACKUP_AGE_RECIPIENTS: ${BACKUP_AGE_RECIPIENTS}
      S3_ENDPOINT: ${S3_ENDPOINT}
      S3_BUCKET: ${S3_BUCKET}
      S3_REGION: ${S3_REGION:-us-east-1}
      S3_ACCESS_KEY_ID: ${S3_ACCESS_KEY_ID}
      S3_SECRET_ACCESS_KEY: ${S3_SECRET_ACCESS_KEY}
      SCHEDULE: "0 3 * * *"
      BACKUP_KEEP_DAYS: "7"
      BACKUP_KEEP_WEEKS: "4"
      BACKUP_KEEP_MONTHS: "6"
      S3_RETENTION_DAYS: "30"
```

---

## 3. Zero-Data-Loss Major Version Upgrade Runbook (16/17/18 -> 19)

1. **Quiesce application writers** so no rows arrive between dump and cutover:
   ```sh
   docker compose stop web worker
   ```
2. **Create a verified Custom-format pre-upgrade dump** from the running old PostgreSQL container:
   ```sh
   docker compose exec -T postgres pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc > /tmp/pre_pg19.dump
   docker compose exec -T postgres pg_restore --list /tmp/pre_pg19.dump >/dev/null # or check non-empty locally
   ls -lh /tmp/pre_pg19.dump
   ```
3. **Update `docker-compose.yml`** to `postgres:19-alpine` (with a new volume `app_pg19_data:/var/lib/postgresql`) and `ghcr.io/phantomwar/ad-postgres-backup:19-alpine`. Keep the old volume untouched as your instant rollback snapshot!
4. **Start only `postgres` 19**, wait for `healthy`, and restore `/tmp/pre_pg19.dump`:
   ```sh
   docker compose up -d postgres
   docker compose exec -T postgres pg_restore -U "$POSTGRES_USER" -d "$POSTGRES_DB" --no-owner --no-privileges < /tmp/pre_pg19.dump
   ```
5. **Run database migrations (`goose` / Prisma / Drizzle), start `web`, `worker`, `db-backup`, and run `restore-test`:**
   ```sh
   docker compose up -d
   docker compose exec db-backup ad-postgres-backup backup
   ```
6. Retain the old volume for at least 7 days before running `docker volume rm <old_pg_volume>`.
