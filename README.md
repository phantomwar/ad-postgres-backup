# ad-postgres-backup

A single-binary Go + Alpine replacement for `prodrigestivill/postgres-backup-local` supporting **PostgreSQL 16, 17, 18, and 19+**, local **GFS hardlink rotation**, **atomic dump verification**, **SHA-256 checksums**, streaming **`age` encryption** (X25519 or scrypt), **S3-compatible offsite sync**, companion **`STORAGE_DIR`** archiving, real **`/healthz` & Prometheus `/metrics`**, and automated **`restore-test`** verification.

## Container Images

Published automatically for `linux/amd64` and `linux/arm64`:

* `ghcr.io/phantomwar/ad-postgres-backup:19-alpine` (also tagged `:latest`, `:alpine`)
* `ghcr.io/phantomwar/ad-postgres-backup:18-alpine`
* `ghcr.io/phantomwar/ad-postgres-backup:17-alpine`
* `ghcr.io/phantomwar/ad-postgres-backup:16-alpine`

Match the tag to your PostgreSQL major version (`postgres:19-alpine` -> `ad-postgres-backup:19-alpine`).

---

## Why Replace `prodrigestivill/postgres-backup-local`?

| Capability | Legacy `postgres-backup-local` | `ad-postgres-backup` |
| :--- | :--- | :--- |
| **PostgreSQL 19 support** | Not published (`18-alpine` max upstream) | **Yes** (`16`, `17`, `18`, `19-alpine` multi-arch) |
| **GFS hardlink rotation** (`last`/`daily`/`weekly`/`monthly`) | Yes | **Yes** (100% compatible folder structure & `*_FILE` vars) |
| **Atomic write + integrity verification** | Writes straight to target | **Writes `.tmp`, checks `gzip` stream or `pg_restore --list` before promoting** |
| **Encryption at rest & offsite** | None | **Streaming [`age`](https://age-encryption.org/) (X25519 asymmetric or scrypt)** |
| **Checksums** | None | **Automatic `.sha256` sidecars** |
| **Offsite S3 sync & retention** | None | **Built-in AWS SDK v2 S3 sync (S3/MinIO/R2/Garage) with fail-closed encryption** |
| **Uploads / attachments backup** | DB only | **Optional `STORAGE_DIR` streaming `.tar.gz` archive + encryption** |
| **Automated restore verification** | None | **Built-in `restore-test` subcommand (ephemeral `initdb` + restore + SQL check)** |
| **Healthcheck** | Static port open only (`go-cron`) | **HTTP `/healthz` (`503` on failure or staleness) + Prometheus `/metrics`** |

---

## Drop-in Migration (Zero Config Changes Required)

Swap the `image:` line and keep your existing `POSTGRES_*`, `SCHEDULE`, `BACKUP_KEEP_*`, `HEALTHCHECK_PORT`, and `/backups` volume:

```yaml
services:
  postgres:
    image: postgres:19-alpine
    volumes:
      - pg19_data:/var/lib/postgresql
    environment:
      POSTGRES_USER: ${POSTGRES_USER}
      POSTGRES_PASSWORD: ${DB_PASS}
      POSTGRES_DB: ${POSTGRES_DB}
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U ${POSTGRES_USER} -d ${POSTGRES_DB}"]
      interval: 10s
      timeout: 5s
      retries: 5

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
      SCHEDULE: "@daily"
      BACKUP_KEEP_DAYS: "7"
      BACKUP_KEEP_WEEKS: "4"
      BACKUP_KEEP_MONTHS: "6"
      HEALTHCHECK_PORT: "8080"
```

Manual one-off backup (both syntaxes work):

```sh
docker exec db-backup ad-postgres-backup backup
# or legacy compatible:
docker exec db-backup /backup.sh
```

---

## Recommended Production Setup (3-2-1 + Asymmetric `age` + S3 + Restore Test)

1. Generate an `age` keypair once on a trusted machine:

```sh
docker run --rm ghcr.io/phantomwar/ad-postgres-backup:19-alpine keygen
```

Keep `BACKUP_AGE_IDENTITY=AGE-SECRET-KEY-1...` **off the server** inside your password manager / secrets vault. Only place the public recipient `BACKUP_AGE_RECIPIENTS=age1...` on the server! Even if the server disk or S3 bucket is read, historical backups cannot be decrypted from the public key.

2. Configure S3 offsite sync + `STORAGE_DIR` + automated `restore-test`:

```yaml
  db-backup:
    image: ghcr.io/phantomwar/ad-postgres-backup:19-alpine
    restart: unless-stopped
    depends_on:
      postgres:
        condition: service_healthy
    volumes:
      - ./backups:/backups
      - app_storage:/storage:ro
    environment:
      POSTGRES_HOST: postgres
      POSTGRES_USER: ${POSTGRES_USER}
      POSTGRES_PASSWORD: ${DB_PASS}
      POSTGRES_DB: ${POSTGRES_DB}
      POSTGRES_EXTRA_OPTS: "-Fc"
      BACKUP_SUFFIX: ".dump"
      STORAGE_DIR: /storage
      BACKUP_AGE_RECIPIENTS: ${BACKUP_AGE_RECIPIENTS}
      S3_ENDPOINT: https://s3.example.com
      S3_BUCKET: app-backups
      S3_REGION: us-east-1
      S3_ACCESS_KEY_ID: ${S3_ACCESS_KEY_ID}
      S3_SECRET_ACCESS_KEY: ${S3_SECRET_ACCESS_KEY}
      S3_RETENTION_DAYS: "30"
      SCHEDULE: "0 3 * * *"
```

3. Verify recoverability anytime (creates an isolated ephemeral PostgreSQL cluster in `/tmp`, never touches production):

```sh
docker compose run --rm -e BACKUP_AGE_IDENTITY="AGE-SECRET-KEY-1..." db-backup restore-test
```

4. Decrypt a backup manually:

```sh
docker run --rm -v "$PWD:/work" -e BACKUP_AGE_IDENTITY="AGE-SECRET-KEY-1..." \
  ghcr.io/phantomwar/ad-postgres-backup:19-alpine decrypt /work/appdb.dump.age /work/appdb.dump
```

---

## Environment Variables Reference

### Drop-in Compatible Variables

| Variable | Default | Description |
| :--- | :--- | :--- |
| `POSTGRES_HOST` | *(required)* | Database host (`POSTGRES_PORT_5432_TCP_ADDR` fallback supported) |
| `POSTGRES_PORT` | `5432` | Database port (`POSTGRES_PORT_5432_TCP_PORT` fallback supported) |
| `POSTGRES_USER` / `POSTGRES_USER_FILE` | *(required)* | PostgreSQL user |
| `POSTGRES_PASSWORD` / `POSTGRES_PASSWORD_FILE` / `POSTGRES_PASSFILE_STORE` | *(required)* | Password or `.pgpass` path |
| `POSTGRES_DB` / `POSTGRES_DB_FILE` | *(required)* | Comma/space/newline-separated database list |
| `POSTGRES_CLUSTER` | `FALSE` | Use `pg_dumpall` + gzip streaming instead of per-DB `pg_dump` |
| `POSTGRES_EXTRA_OPTS` | `-Z6` (when `.gz`) | Extra `pg_dump` / `pg_dumpall` flags (supports quoted args) |
| `SCHEDULE` | `@daily` | `@daily`, `@hourly`, `@every 6h`, 5-field cron, or 6-field `go-cron` |
| `BACKUP_ON_START` | `FALSE` | Run one cycle immediately when daemon starts |
| `VALIDATE_ON_START` | `TRUE` | Validate required env & writable `BACKUP_DIR` on startup |
| `BACKUP_DIR` | `/backups` | Backup root directory (POSIX filesystem required for hardlinks) |
| `BACKUP_SUFFIX` | `.sql.gz` | Output dump suffix (e.g. `.sql.gz` or `.dump` with `-Fc`) |
| `BACKUP_LATEST_TYPE` | `symlink` | `symlink`, `hardlink`, or `none` |
| `BACKUP_KEEP_MINS` | `1440` | Retention minutes in `last/` |
| `BACKUP_KEEP_DAYS` | `7` | Retention days in `daily/` |
| `BACKUP_KEEP_WEEKS` | `4` | Retention weeks in `weekly/` |
| `BACKUP_KEEP_MONTHS` | `6` | Retention months in `monthly/` |
| `HEALTHCHECK_PORT` | `8080` | Port for `/`, `/healthz`, and `/metrics` |
| `HOOKS_DIR` | `/hooks` | Executable scripts receiving `pre-backup`, `post-backup`, or `error` |
| `WEBHOOK_URL` / `WEBHOOK_ERROR_URL` / `WEBHOOK_PRE_BACKUP_URL` / `WEBHOOK_POST_BACKUP_URL` | *(empty)* | JSON webhook URLs |

### Security, Offsite S3 & Verification Variables

| Variable | Default | Description |
| :--- | :--- | :--- |
| `BACKUP_CHECKSUM` | `TRUE` | Generate `.sha256` sidecars alongside every artifact |
| `DISK_MAX_USAGE_PCT` | `92` | Abort dump if filesystem usage on `BACKUP_DIR` is `>=` threshold |
| `PRUNE_BEFORE_DUMP` | `FALSE` | Prune expired files before starting a dump (normally after success) |
| `BACKUP_MAX_STALE_HOURS` | `36` | Mark `/healthz` unhealthy (`503`) if no successful backup within N hours |
| `BACKUP_AGE_RECIPIENTS` / `*_FILE` | *(empty)* | One or more `age1...` public keys for asymmetric encryption |
| `BACKUP_AGE_IDENTITY` / `*_FILE` | *(empty)* | Secret `AGE-SECRET-KEY-1...` identity for `decrypt` / `restore-test` |
| `BACKUP_PASSWORD` / `*_FILE` | *(empty)* | Symmetric `age` scrypt passphrase (alternative to X25519 keys) |
| `STORAGE_DIR` | *(empty)* | Optional attachments directory to archive as `<STORAGE_PREFIX>-*.tar.gz` |
| `STORAGE_PREFIX` | `storage` | Filename prefix for `STORAGE_DIR` archives |
| `S3_BUCKET` | *(empty)* | Enable S3 offsite upload when non-empty |
| `S3_ENDPOINT` | *(empty)* | Custom S3 endpoint (for MinIO, Garage, R2, etc.) |
| `S3_REGION` / `AWS_DEFAULT_REGION` | `us-east-1` | S3 signing region |
| `S3_PREFIX` | *(empty)* | Optional object key prefix inside the bucket |
| `S3_ACCESS_KEY_ID` / `AWS_ACCESS_KEY_ID` | *(empty)* | Static S3 access key (also supports `*_FILE`) |
| `S3_SECRET_ACCESS_KEY` / `AWS_SECRET_ACCESS_KEY` | *(empty)* | Static S3 secret key (also supports `*_FILE`) |
| `S3_FORCE_PATH_STYLE` | `TRUE` | Use path-style S3 URLs (`endpoint/bucket/key`) |
| `S3_RETENTION_DAYS` | `30` | Delete remote S3 objects older than N days |
| `S3_REQUIRE_ENCRYPTION` | `TRUE` | Fail-closed: refuse S3 uploads unless `age` or passphrase encryption is enabled |
| `RESTORE_TEST_QUERY` | `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'` | SQL sanity query run during `restore-test` |
| `RESTORE_TEST_PORT` | `55432` | Ephemeral localhost PostgreSQL port for `restore-test` |

For step-by-step PostgreSQL 16/17/18 -> 19 migration instructions, see [docs/STANDARDIZATION_GUIDE.md](docs/STANDARDIZATION_GUIDE.md).
