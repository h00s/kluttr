# Kluttr Backend — Technical Specification

> **Version:** 0.1.0 (initial draft)
> **Status:** Approved design, pending implementation
> **Last updated:** 2026-05-16
> **Audience:** Backend engineers and AI coding assistants implementing the Kluttr API.

This document is the source of truth for the Kluttr backend. It is framework-agnostic in spirit but concrete in choices: every architectural decision is locked, and patterns mirror the user's existing Raptor v4 project (`schoolbell`) for consistency.

For repo orientation, see `CLAUDE.md` at the repo root. For deferred features, see §20.

---

## Table of Contents

1. [Overview](#1-overview)
2. [Architecture](#2-architecture)
3. [Technology Stack](#3-technology-stack)
4. [Project Layout](#4-project-layout)
5. [Configuration](#5-configuration)
6. [Identity & Authentication](#6-identity--authentication)
7. [Data Model](#7-data-model)
8. [Content Pipelines](#8-content-pipelines)
9. [Lists, Tags, Highlights](#9-lists-tags-highlights)
10. [Search](#10-search)
11. [LLM Integration](#11-llm-integration)
12. [File Storage](#12-file-storage)
13. [Background Jobs](#13-background-jobs)
14. [API Conventions](#14-api-conventions)
15. [Endpoint Catalog](#15-endpoint-catalog)
16. [Public Sharing](#16-public-sharing)
17. [Soft-Delete & Trash](#17-soft-delete--trash)
18. [Observability](#18-observability)
19. [Security Checklist](#19-security-checklist)
20. [Deferred / Future Work](#20-deferred--future-work)
21. [Appendix](#21-appendix)

---

## 1. Overview

### 1.1 What Kluttr is

Kluttr is a **self-hostable, multi-user, "bookmark everything" web application**. Users save links, take simple notes, write markdown documents, and store images and PDFs. Anything can be organised into lists, tagged (manually or automatically by an LLM), full-text-searched, and optionally shared with the world as read-only public pages.

Kluttr is inspired by Hoarder but is **deliberately smaller**. It implements the core of what Hoarder does, does it fast, and refuses to grow a long-tail of features.

### 1.2 Target user and hosting model

A Kluttr instance is **self-hosted by an individual or small group**. One Postgres database, one Go binary, one optional object store. **Multiple users** can register on a single instance, but there is **no tenant isolation beyond user_id scoping** — every instance is one community of users who chose to share infrastructure.

There is no built-in sharing or collaboration between users. The only cross-user surface is the **public read-only view**: any user can mark an item as public and share its URL with the world.

### 1.3 Goals (in priority order)

1. **Core feature parity** with Hoarder for links, notes, markdown, images, PDFs, lists, tags, search, LLM tagging/summarisation.
2. **Fast and lightweight** — single binary plus Postgres; no Redis, no external queue, no external search engine.
3. **Pleasant to develop** — clear conventions, predictable layout, generated migrations from typed models.
4. **Secure by default** — modern password hashing, opaque tokens, rate limits, SSRF protection on URL fetches.
5. **Operable** — health endpoints, structured logs, graceful shutdown.

### 1.4 Explicit non-goals (v1)

- Sharing/collaboration between authenticated users.
- API keys, browser extensions, mobile apps, CLI — the v1 surface is the API consumed by the official web frontend.
- Imports from other tools (Pocket, browser HTML, Linkwarden, Omnivore, …).
- Full-page archival (monolith), video archiving (yt-dlp), OCR, RSS auto-ingest.
- Semantic / vector search.
- WebSocket / SSE for live updates — clients poll.
- SSO, multi-language UI strings (English-only API errors).
- Versioning of notes/markdown.
- Account export / GDPR data dump.

Each of these has a place in the v2 backlog (§20).

---

## 2. Architecture

### 2.1 Component map

```
┌──────────────────────────────────────────────────────────────────────────┐
│  HTTP layer (Raptor + middlewares)                                       │
│    logger ─ cors ─ auth (skip for /auth/login, /auth/register, /p/*,     │
│                            /healthz, /readyz, /version)                  │
└────────────────────────┬─────────────────────────────────────────────────┘
                         │
                ┌────────▼────────┐
                │   Controllers   │  thin: bind, validate, call service,
                │  one per route  │        shape response
                │     group       │
                └────────┬────────┘
                         │
        ┌────────────────┼────────────────┬─────────────────┐
        │                │                │                 │
   ┌────▼────┐      ┌────▼─────┐     ┌────▼─────┐      ┌────▼─────┐
   │Services │      │ Storage  │     │   LLM    │      │ Jobs     │
   │(business│      │interface │     │ provider │      │ enqueuer │
   │  logic) │      │ (local/  │     │interface │      │          │
   │         │      │   s3)    │     │(openai/  │      │          │
   │         │      │          │     │ ollama)  │      │          │
   └────┬────┘      └────┬─────┘     └────┬─────┘      └────┬─────┘
        │                │                │                 │
        │       ┌────────▼───────┐        │                 │
        │       │ Filesystem /   │        │                 │
        │       │ S3-compatible  │        │                 │
        │       └────────────────┘        │                 │
        │                                 │                 │
   ┌────▼─────────────────────────────────▼─────────────────▼─────┐
   │                         PostgreSQL                            │
   │   users  sessions  items  *_details  lists  list_items        │
   │   tags  item_tags  highlights  jobs                           │
   └────────────────────────────┬──────────────────────────────────┘
                                │
                       ┌────────▼─────────┐
                       │  Worker pool     │  N goroutines, SKIP LOCKED
                       │  (in-process)    │  polling on jobs table
                       └──────────────────┘
```

### 2.2 Request lifecycle (authenticated)

1. Client sends `Authorization: Bearer <token>` to `/api/v1/...`.
2. **Logger middleware** records start/end + status + latency.
3. **CORS middleware** validates origin against `cors_allow_origins`.
4. **Auth middleware** parses Bearer, looks up session (cache-first, DB fallback), rejects on miss/expired, injects `*Session` into request context.
5. **Controller** binds JSON to a request struct, runs Zog validation, dispatches to the appropriate service method, shapes the response.
6. **Service** runs business logic against the DB through Bun, returns domain objects or typed errors.
7. **Errors** flow up as `errs.Error*` values that Raptor translates to standard JSON error responses (§14.4).

### 2.3 Async work lifecycle

Services that need work done off the request path **enqueue a job row** in the `jobs` table inside the same transaction as the originating data write (so a successful enqueue implies a successful write).

A pool of **worker goroutines** started in the `JobsService.Setup()` hook polls the `jobs` table with `SELECT ... FOR UPDATE SKIP LOCKED`, runs the handler, and either marks the job `succeeded` or schedules a retry with exponential backoff up to `max_attempts`. On `SIGTERM` the pool stops claiming new jobs and waits up to `shutdown_timeout` seconds for in-flight jobs to drain.

### 2.4 Why no external queue?

Postgres + `SKIP LOCKED` gives transactional enqueue, durable storage, and good-enough throughput for a self-hosted personal app. Adding Redis or NATS would buy throughput we don't need and operational cost we don't want. If the workload outgrows this, the abstraction is small enough to swap (see §13.6).

---

## 3. Technology Stack

| Concern | Choice | Module path | Notes |
|---|---|---|---|
| Framework | Raptor v4 | `github.com/go-raptor/raptor/v4` | Already scaffolded. |
| HTTP middlewares | Raptor middlewares | `github.com/go-raptor/middlewares/{cors,logger}` | Plus our `AuthMiddleware`. |
| DB connector | Bun + Postgres | `github.com/go-raptor/connectors/bun/postgres` | Registers as `Components.DatabaseConnector`. |
| ORM / query builder | Bun | `github.com/uptrace/bun` | With `dialect/pgdialect`. |
| Driver | pgx v5 | `github.com/jackc/pgx/v5` | Used by Bun under the hood. |
| Migrations | Goose | `github.com/pressly/goose/v3` | Go-based, one file per change. |
| Validation | Zog | `github.com/Oudwins/zog` | Schemas live next to models. |
| In-memory cache | Ristretto v2 | `github.com/dgraph-io/ristretto/v2` | Session lookups, hot reads. |
| Password hashing | Argon2id | `golang.org/x/crypto/argon2` | See §6.4 for parameters. |
| Logging | slog + tint | `log/slog` + `github.com/lmittmann/tint` | Coloured dev output, plain JSON in prod. |
| HTTP client (link fetcher) | stdlib `net/http` | — | With timeouts, redirect cap, SSRF guard. |
| HTML metadata parsing | `golang.org/x/net/html` | — | OpenGraph + `<title>` + `<meta>` parsing. |
| Image processing | `image`, `image/jpeg`, `image/png`, optionally `golang.org/x/image` | — | Thumbnails only — no fancy effects. |
| S3 client | AWS SDK v2 or minio-go | TBD at impl | Pick whichever is leaner; both work with S3/MinIO/R2. |
| OpenAI client | Native HTTP against `/v1/chat/completions` | — | No SDK dependency. |
| Ollama client | Native HTTP against `/api/chat` | — | No SDK dependency. |

**Constraint:** every dependency above is either already in `schoolbell` or is a leaf-level addition with no transitive bloat. We do **not** add general-purpose web frameworks, ORMs, or job queue libraries beyond this list.

---

## 4. Project Layout

The backend follows the same structure as `schoolbell/backend` exactly. New top-level directories are added only when justified (e.g. `app/jobs/` for job handlers, `app/storage/` for storage adapters, `app/llm/` for LLM adapters).

```
backend/
├── main.go                            # raptor.New(...).Run() — unchanged from scaffold
├── go.mod
├── .raptor.dev.yaml
├── .raptor.example.yaml
├── .raptor.test.yaml
├── README.md
├── app/
│   ├── controllers/                   # one file per resource
│   │   ├── auth_controller.go
│   │   ├── items_controller.go
│   │   ├── bookmarks_controller.go
│   │   ├── notes_controller.go
│   │   ├── files_controller.go
│   │   ├── lists_controller.go
│   │   ├── tags_controller.go
│   │   ├── highlights_controller.go
│   │   ├── search_controller.go
│   │   ├── public_controller.go
│   │   └── system_controller.go       # /healthz /readyz /version
│   ├── middlewares/
│   │   └── auth_middleware.go         # Bearer → Session
│   ├── models/                        # one file per model, includes Bun struct + Request/Response + Zog schema
│   │   ├── user.go
│   │   ├── session.go
│   │   ├── item.go
│   │   ├── bookmark_detail.go
│   │   ├── note_detail.go
│   │   ├── file_detail.go
│   │   ├── list.go
│   │   ├── list_item.go
│   │   ├── tag.go
│   │   ├── item_tag.go
│   │   ├── highlight.go
│   │   └── job.go
│   ├── services/                      # business logic, one file per resource + cross-cutting
│   │   ├── database_service.go        # mirrors schoolbell
│   │   ├── cache_service.go           # ristretto wrapper
│   │   ├── validation_service.go      # holds compiled Zog schemas
│   │   ├── auth_service.go
│   │   ├── users_service.go
│   │   ├── sessions_service.go
│   │   ├── items_service.go
│   │   ├── bookmarks_service.go
│   │   ├── notes_service.go
│   │   ├── files_service.go
│   │   ├── lists_service.go
│   │   ├── tags_service.go
│   │   ├── highlights_service.go
│   │   ├── search_service.go
│   │   ├── jobs_service.go            # enqueue + worker pool
│   │   ├── storage_service.go         # wraps the Storage interface
│   │   └── llm_service.go             # wraps the LLMProvider interface
│   ├── storage/                       # Storage interface + implementations
│   │   ├── storage.go                 # interface
│   │   ├── local.go                   # local filesystem adapter
│   │   └── s3.go                      # S3-compatible adapter
│   ├── llm/                           # LLMProvider interface + adapters
│   │   ├── llm.go                     # interface, prompt builders
│   │   ├── openai.go
│   │   └── ollama.go
│   ├── jobs/                          # job handlers — pure functions
│   │   ├── fetch_link_metadata.go
│   │   ├── generate_thumbnail.go
│   │   ├── llm_tag_summarize.go
│   │   ├── purge_trash.go
│   │   └── cleanup_expired_sessions.go
│   └── utils/
│       ├── urlnorm.go                 # URL normalisation + hashing for dedup
│       ├── slug.go                    # base62 public-slug generator
│       └── httpfetch.go               # safe HTTP fetcher (SSRF guard, redirect cap)
├── config/
│   ├── routes.go                      # embeds routes.yaml
│   ├── routes.yaml
│   └── components/
│       ├── components.go              # New() wires DatabaseConnector + services + middlewares + controllers
│       ├── controllers.go
│       ├── services.go
│       └── middlewares.go
└── db/
    ├── migrations.go                  # MigrationsFS() — currently returns nil; init()s register migrations
    └── migrations/                    # YYYYMMDDHHMMSS_<name>.go
        ├── 20260516120001_create_users.go
        ├── 20260516120002_create_sessions.go
        ├── 20260516120003_create_items.go
        ├── 20260516120004_create_bookmark_details.go
        ├── 20260516120005_create_note_details.go
        ├── 20260516120006_create_file_details.go
        ├── 20260516120007_create_lists.go
        ├── 20260516120008_create_list_items.go
        ├── 20260516120009_create_tags.go
        ├── 20260516120010_create_item_tags.go
        ├── 20260516120011_create_highlights.go
        ├── 20260516120012_create_jobs.go
        ├── 20260516120013_enable_pg_trgm.go
        ├── 20260516120014_add_items_search_tsv.go
        └── 20260516120015_add_foreign_keys.go
```

---

## 5. Configuration

Raptor reads `.raptor.<env>.yaml` (e.g. `.raptor.dev.yaml`, `.raptor.test.yaml`, or `.raptor.production.yaml`). Kluttr extends the standard schema under the `app:` section.

```yaml
general:
  log_level: debug              # debug | info | warn | error

server:
  address: "0.0.0.0"
  port: 3000
  shutdown_timeout: 10

database:
  host: localhost
  port: 5432
  username: kluttr
  password: secret
  name: kluttr_development

app:
  # CORS
  cors_allow_origins: "http://localhost:5173"

  # Public URLs (used in password-reset links and the absolute slug URL)
  public_base_url: "http://localhost:3000"

  # Account policy
  signup_enabled: true
  password_min_length: 10
  session_ttl_hours: 720         # 30 days; 0 = never expire
  max_sessions_per_user: 25

  # Trash retention
  trash_purge_days: 30

  # Workers
  worker_concurrency: 4
  worker_poll_interval_ms: 1000
  job_max_attempts: 5

  # Storage backend
  storage_backend: "local"       # "local" | "s3"
  storage_local_path: "./data/storage"
  storage_max_upload_mb: 50
  storage_signed_url_ttl_seconds: 300
  # S3-only (ignored when backend = local)
  storage_s3_endpoint: ""        # e.g. "https://s3.amazonaws.com" or MinIO URL
  storage_s3_region: "us-east-1"
  storage_s3_bucket: "kluttr"
  storage_s3_prefix: "kluttr/"
  storage_s3_access_key: ""
  storage_s3_secret_key: ""
  storage_s3_force_path_style: false

  # LLM provider
  llm_enabled: true
  llm_provider: "ollama"         # "openai" | "ollama"
  llm_model: "llama3.1:8b"
  llm_request_timeout_seconds: 60
  llm_max_input_chars: 12000
  # OpenAI-only
  llm_openai_base_url: "https://api.openai.com/v1"
  llm_openai_api_key: ""
  # Ollama-only
  llm_ollama_base_url: "http://localhost:11434"

  # Link fetcher
  link_fetch_timeout_seconds: 15
  link_fetch_max_bytes: 5242880  # 5 MB
  link_fetch_user_agent: "KluttrBot/1.0"

  # SMTP (optional; password reset)
  smtp_enabled: false
  smtp_host: ""
  smtp_port: 587
  smtp_username: ""
  smtp_password: ""
  smtp_from: "no-reply@example.com"

  # Rate limits (per minute)
  rate_limit_auth_per_ip: 30
  rate_limit_api_per_user: 600
  rate_limit_public_per_ip: 120
```

Every key above is consumed somewhere in this spec; cross-references are noted at the point of use.

---

## 6. Identity & Authentication

### 6.1 Auth model summary

- A user signs up with **email + password**.
- A successful login creates a **session row** and returns an **opaque token** to the client.
- The client sends `Authorization: Bearer <token>` on every subsequent request.
- The server **never stores the plaintext token**; only `sha256(token)` is stored.
- There is **no JWT**, no refresh token, no OAuth in v1.
- Sessions live until they expire or the user logs out / changes password.

This mirrors the `schoolbell` pattern (see `app/services/sessions_service.go`) exactly, with the addition of email-based identity and Argon2id for passwords.

### 6.2 Endpoints

See §15.1 for full request/response schemas.

| Method | Path | Purpose | Auth required |
|---|---|---|---|
| `POST` | `/api/v1/auth/register` | Create a user (when `signup_enabled`) | No |
| `POST` | `/api/v1/auth/login` | Exchange email + password for a session token | No |
| `POST` | `/api/v1/auth/logout` | Delete the current session | Yes |
| `GET`  | `/api/v1/auth/me` | Return the current user | Yes |
| `POST` | `/api/v1/auth/password` | Change password (requires current password) | Yes |
| `POST` | `/api/v1/auth/password/reset` | Email a reset token (requires SMTP) | No |
| `POST` | `/api/v1/auth/password/reset/confirm` | Set a new password using the emailed token | No |

### 6.3 Session token lifecycle

```
client                     server                      database
  │   POST /auth/login        │                          │
  │ ─ email, password ──────▶ │                          │
  │                           │   SELECT users WHERE     │
  │                           │   email = ?              │
  │                           │ ────────────────────────▶│
  │                           │ ◀──── user row ──────────│
  │                           │                          │
  │                           │ argon2.Verify(password)  │
  │                           │                          │
  │                           │ token = base64url(rand32)│
  │                           │ hash  = sha256(token)    │
  │                           │ INSERT INTO sessions     │
  │                           │   (user_id, token=hash,  │
  │                           │    ip, expires_at)       │
  │                           │ ────────────────────────▶│
  │                           │                          │
  │ ◀ {token, user} ──────────│                          │
```

- **Generation:** 32 random bytes from `crypto/rand`, base64-RawURL-encoded → ~43 chars.
- **Storage:** sha256 hex (64 chars) in the `sessions.token` column (named `token` to match `schoolbell`, but it holds the *hash*).
- **Verification (auth middleware):** parse `Authorization: Bearer <plaintext>`, compute `sha256(plaintext)`, look up by hash in cache (`sessions:<hash>` key), then DB fallback.
- **Cache:** Ristretto with TTL = `session_ttl_hours` (or 24h if 0). On `Delete`, the cache is invalidated for that hash.
- **Pruning:** On each new session insert, a goroutine prunes any sessions beyond `max_sessions_per_user` for that user, ordered by `created_at DESC` (most recent kept). This matches `schoolbell/app/services/sessions_service.go:pruneOldSessions`.
- **Expiration:** If `session_ttl_hours > 0`, `expires_at = now() + ttl`. Auth middleware rejects expired sessions.
- **Password change** invalidates all existing sessions for that user.

### 6.4 Password hashing

**Algorithm:** Argon2id (`golang.org/x/crypto/argon2`).

**Parameters (target ~50–100ms on commodity hardware):**

| Parameter | Value |
|---|---|
| time (iterations) | 2 |
| memory | 64 MiB (`64 * 1024` KiB) |
| parallelism | 2 |
| key length | 32 bytes |
| salt length | 16 bytes (per-password, from `crypto/rand`) |

**Encoded form:** standard Argon2id encoded string
`$argon2id$v=19$m=65536,t=2,p=2$<salt-b64>$<hash-b64>`

This is stored in `users.password`. Verification re-derives with the parameters embedded in the encoded string, so we can rotate parameters later without breaking existing users.

> **Implementation note:** `schoolbell` uses bcrypt. The user accepted the Argon2id upgrade during planning. If consistency with `schoolbell` is preferred over the upgrade, switch to `golang.org/x/crypto/bcrypt` with cost ≥ 12 — the rest of the spec is unaffected.

### 6.5 Password policy

- **Minimum length:** `password_min_length` (default 10). No complexity rules; length wins.
- **No reuse / history checks** in v1.
- **Reset:** if `smtp_enabled`, `POST /auth/password/reset` emails a single-use token (signed, 1-hour TTL) stored in a `password_reset_tokens` table — *or*, to avoid a new table, we store one ad-hoc row in `sessions` with a `purpose='reset'` column. **Chosen:** add a small `password_reset_tokens` table; sessions remain pure (see §7.2 for schema).

### 6.6 Email verification

Off in v1. The `users.email_verified_at` column is nullable and reserved for v2. Registration immediately activates the account.

### 6.7 Auth middleware

Single middleware (`AuthMiddleware`) applied to every route via `core.UseExcept(...)`, with the following routes excluded:

- `Auth.Register`, `Auth.Login`
- `Auth.PasswordReset`, `Auth.PasswordResetConfirm`
- `Public.Show` (`GET /p/:slug`)
- `System.Health`, `System.Ready`, `System.Version`

Mirrors `schoolbell/config/components/middlewares.go`.

### 6.8 Rate limiting on auth

Auth endpoints are wrapped in an IP-based rate limiter (token bucket, `rate_limit_auth_per_ip` per minute, burst = 5). On exceedance, return `429 Too Many Requests` with a `Retry-After` header. Implementation is a small in-process limiter (no extra dep); state is fine in-memory because instances are single-process.

---

## 7. Data Model

This section is the **schema source of truth**. Each table has:

1. Purpose
2. Bun model (`app/models/<name>.go`) — the struct from which Goose+Bun derive the migration DDL.
3. Indexes and constraints (added in raw SQL in the migration where Bun tags can't express them).
4. Access notes.

All tables use:

- `id BIGSERIAL PRIMARY KEY` (Bun: `bun:"id,pk,autoincrement"`).
- `created_at TIMESTAMPTZ NOT NULL DEFAULT now()`.
- `updated_at TIMESTAMPTZ NOT NULL DEFAULT now()` — updated by service code on writes.
- Where applicable, `deleted_at TIMESTAMPTZ` — nullable; non-null means soft-deleted.

Time columns use `time.Time` with `bun:"...,nullzero,notnull,default:current_timestamp"` (matches `schoolbell`).

> **Migration ordering:** tables are created in the order listed (parent before child where possible). Foreign keys are added in a **single final migration** so the order is decoupled from FK dependencies. This matches `schoolbell/db/migrations/20260509110801_add_foreign_keys.go`.

### 7.1 `users`

**Purpose:** account record.

```go
type User struct {
    bun.BaseModel `bun:"table:users,alias:users"`

    ID              int64      `bun:"id,pk,autoincrement"        json:"id"`
    Email           string     `bun:"email,notnull,unique"       json:"email"`
    Password        string     `bun:"password,notnull"           json:"-"`
    DisplayName     string     `bun:"display_name"               json:"displayName"`
    EmailVerifiedAt *time.Time `bun:"email_verified_at,nullzero" json:"emailVerifiedAt,omitempty"`
    CreatedAt       time.Time  `bun:"created_at,nullzero,notnull,default:current_timestamp" json:"createdAt"`
    UpdatedAt       time.Time  `bun:"updated_at,nullzero,notnull,default:current_timestamp" json:"updatedAt"`
}
```

**Indexes:** unique on `email` (via tag). Lowercase the email on insert/update — store canonical form.

**Notes:** `password` is the Argon2id encoded string (§6.4). `display_name` is optional; defaults to the local-part of the email.

### 7.2 `sessions` and `password_reset_tokens`

**`sessions`** — opaque bearer-token sessions.

```go
type Session struct {
    bun.BaseModel `bun:"table:sessions,alias:sessions"`

    ID        int64      `bun:"id,pk,autoincrement"   json:"id"`
    UserID    int64      `bun:"user_id,notnull"       json:"userId"`
    TokenHash string     `bun:"token,notnull,unique"  json:"-"`
    IPAddress string     `bun:"ip_address"            json:"ipAddress,omitempty"`
    UserAgent string     `bun:"user_agent"            json:"userAgent,omitempty"`
    ExpiresAt *time.Time `bun:"expires_at,nullzero"   json:"expiresAt,omitempty"`
    CreatedAt time.Time  `bun:"created_at,nullzero,notnull,default:current_timestamp" json:"createdAt"`
    UpdatedAt time.Time  `bun:"updated_at,nullzero,notnull,default:current_timestamp" json:"updatedAt"`

    User *User `bun:"rel:belongs-to,join:user_id=id" json:"user,omitempty"`
}
```

**Indexes:** unique on `token` (the hash). Index on `user_id` for pruning.
**FK:** `user_id` → `users(id)` `ON DELETE CASCADE`.

**`password_reset_tokens`** — single-use reset tokens.

```go
type PasswordResetToken struct {
    bun.BaseModel `bun:"table:password_reset_tokens,alias:prt"`

    ID        int64     `bun:"id,pk,autoincrement"   json:"-"`
    UserID    int64     `bun:"user_id,notnull"       json:"-"`
    TokenHash string    `bun:"token,notnull,unique"  json:"-"`
    ExpiresAt time.Time `bun:"expires_at,notnull"    json:"-"`
    UsedAt    *time.Time `bun:"used_at,nullzero"     json:"-"`
    CreatedAt time.Time `bun:"created_at,nullzero,notnull,default:current_timestamp" json:"-"`
}
```

**FK:** `user_id` → `users(id)` `ON DELETE CASCADE`.
**Notes:** TTL = 1 hour. Marked `used_at` on consumption; cleaned by `cleanup_expired_sessions` job (also handles these).

### 7.3 `items` (shared content row)

**Purpose:** one row per piece of content — link, note, markdown doc, image, PDF — holding shared fields. Type-specific fields live in detail tables (§7.4–7.6).

```go
type Item struct {
    bun.BaseModel `bun:"table:items,alias:items"`

    ID          int64      `bun:"id,pk,autoincrement"      json:"id"`
    UserID      int64      `bun:"user_id,notnull"          json:"userId"`
    Type        string     `bun:"type,notnull"             json:"type"`         // "bookmark" | "note" | "markdown" | "image" | "pdf"
    Title       string     `bun:"title"                    json:"title"`
    Summary     string     `bun:"summary"                  json:"summary"`
    AICategory  string     `bun:"ai_category"              json:"aiCategory,omitempty"`
    Pinned      bool       `bun:"pinned,notnull,default:false" json:"pinned"`
    IsPublic    bool       `bun:"is_public,notnull,default:false" json:"isPublic"`
    PublicSlug  string     `bun:"public_slug,unique,nullzero" json:"publicSlug,omitempty"`
    SearchText  string     `bun:"search_text"              json:"-"`            // denormalised aggregate; see §10
    State       string     `bun:"state,notnull,default:'ready'" json:"state"`   // "pending" | "ready" | "error"
    DeletedAt   *time.Time `bun:"deleted_at,nullzero,soft_delete" json:"deletedAt,omitempty"`
    CreatedAt   time.Time  `bun:"created_at,nullzero,notnull,default:current_timestamp" json:"createdAt"`
    UpdatedAt   time.Time  `bun:"updated_at,nullzero,notnull,default:current_timestamp" json:"updatedAt"`

    // Relations (loaded on demand)
    BookmarkDetail *BookmarkDetail `bun:"rel:has-one,join:id=item_id" json:"bookmark,omitempty"`
    NoteDetail     *NoteDetail     `bun:"rel:has-one,join:id=item_id" json:"note,omitempty"`
    FileDetail     *FileDetail     `bun:"rel:has-one,join:id=item_id" json:"file,omitempty"`
    Tags           []Tag           `bun:"m2m:item_tags,join:Item=Tag" json:"tags,omitempty"`
}
```

**`type` enum (CHECK constraint):** `('bookmark','note','markdown','image','pdf')`.
**`state` enum (CHECK constraint):** `('pending','ready','error')` — `pending` while metadata/LLM jobs are running.

**Indexes:**

- `(user_id, deleted_at, created_at DESC)` — primary list query.
- `(user_id, type, deleted_at)` — type filter.
- `(user_id, pinned, created_at DESC) WHERE pinned = true` — partial index for pinned-first lists.
- `(public_slug) WHERE is_public = true` — unique partial; lookup for public view.
- A generated stored column `search_tsv` (see §10) with GIN index.

**FK:** `user_id` → `users(id)` `ON DELETE CASCADE`.

**Notes:** `bun:"...,soft_delete"` makes Bun's default queries filter out soft-deleted rows automatically. We override that filter in trash queries.

### 7.4 `bookmark_details`

```go
type BookmarkDetail struct {
    bun.BaseModel `bun:"table:bookmark_details,alias:bd"`

    ItemID         int64      `bun:"item_id,pk"               json:"itemId"`
    URL            string     `bun:"url,notnull"              json:"url"`
    URLHash        string     `bun:"url_hash,notnull"         json:"-"`           // sha256(normalised URL)
    Domain         string     `bun:"domain"                   json:"domain"`
    SiteName       string     `bun:"site_name"                json:"siteName,omitempty"`
    Author         string     `bun:"author"                   json:"author,omitempty"`
    Description    string     `bun:"description"              json:"description,omitempty"`
    ImageURL       string     `bun:"image_url"                json:"imageUrl,omitempty"`
    FaviconURL     string     `bun:"favicon_url"              json:"faviconUrl,omitempty"`
    ThumbnailFileID *int64    `bun:"thumbnail_file_id,nullzero" json:"thumbnailFileId,omitempty"` // FK to file_details.item_id
    PublishedAt    *time.Time `bun:"published_at,nullzero"    json:"publishedAt,omitempty"`
    FetchedAt      *time.Time `bun:"fetched_at,nullzero"      json:"fetchedAt,omitempty"`
    HTTPStatus     int        `bun:"http_status"              json:"httpStatus,omitempty"`
    CreatedAt      time.Time  `bun:"created_at,nullzero,notnull,default:current_timestamp" json:"createdAt"`
    UpdatedAt      time.Time  `bun:"updated_at,nullzero,notnull,default:current_timestamp" json:"updatedAt"`
}
```

**Indexes:**

- Partial unique on `(user_id, url_hash)` for duplicate detection. Because `bookmark_details` doesn't carry `user_id`, the index is created as a partial expression index on `items` joined to `bookmark_details`, OR `user_id` is denormalised onto `bookmark_details`. **Chosen:** denormalise `user_id` onto `bookmark_details` to keep the index local and FK enforcement simple. Add `UserID int64 bun:"user_id,notnull"`.
- Index on `domain` for "all my github.com links" type queries.

**FK:** `item_id` → `items(id)` `ON DELETE CASCADE`.

### 7.5 `note_details`

```go
type NoteDetail struct {
    bun.BaseModel `bun:"table:note_details,alias:nd"`

    ItemID    int64     `bun:"item_id,pk"   json:"itemId"`
    Format    string    `bun:"format,notnull,default:'plain'" json:"format"` // "plain" | "markdown"
    Body      string    `bun:"body,notnull" json:"body"`
    CreatedAt time.Time `bun:"created_at,nullzero,notnull,default:current_timestamp" json:"createdAt"`
    UpdatedAt time.Time `bun:"updated_at,nullzero,notnull,default:current_timestamp" json:"updatedAt"`
}
```

**CHECK:** `format IN ('plain','markdown')`.
**Notes:** Body is plain text or markdown source; we do not store rendered HTML. The frontend renders markdown.

### 7.6 `file_details`

```go
type FileDetail struct {
    bun.BaseModel `bun:"table:file_details,alias:fd"`

    ItemID           int64     `bun:"item_id,pk"             json:"itemId"`
    StorageKey       string    `bun:"storage_key,notnull,unique" json:"-"`           // opaque key in the Storage backend
    OriginalFilename string    `bun:"original_filename"      json:"originalFilename"`
    MimeType         string    `bun:"mime_type,notnull"      json:"mimeType"`
    SizeBytes        int64     `bun:"size_bytes,notnull"     json:"sizeBytes"`
    Sha256           string    `bun:"sha256,notnull"         json:"sha256"`
    Width            int       `bun:"width"                  json:"width,omitempty"`
    Height           int       `bun:"height"                 json:"height,omitempty"`
    PageCount        int       `bun:"page_count"             json:"pageCount,omitempty"`         // PDFs
    ThumbnailKey     string    `bun:"thumbnail_key"          json:"-"`                          // optional thumb path in storage
    CreatedAt        time.Time `bun:"created_at,nullzero,notnull,default:current_timestamp" json:"createdAt"`
    UpdatedAt        time.Time `bun:"updated_at,nullzero,notnull,default:current_timestamp" json:"updatedAt"`
}
```

**Indexes:** unique on `storage_key`.
**Notes:** When `item.type` is `image` or `pdf`, this row is required. `Sha256` enables future server-side dedup (not enforced in v1 to keep behaviour predictable for the user).

### 7.7 `lists`

```go
type List struct {
    bun.BaseModel `bun:"table:lists,alias:lists"`

    ID          int64      `bun:"id,pk,autoincrement"      json:"id"`
    UserID      int64      `bun:"user_id,notnull"          json:"userId"`
    Name        string     `bun:"name,notnull"             json:"name"`
    Description string     `bun:"description"              json:"description,omitempty"`
    Icon        string     `bun:"icon"                     json:"icon,omitempty"`
    Color       string     `bun:"color"                    json:"color,omitempty"`
    IsPublic    bool       `bun:"is_public,notnull,default:false" json:"isPublic"`
    PublicSlug  string     `bun:"public_slug,unique,nullzero" json:"publicSlug,omitempty"`
    DeletedAt   *time.Time `bun:"deleted_at,nullzero,soft_delete" json:"deletedAt,omitempty"`
    CreatedAt   time.Time  `bun:"created_at,nullzero,notnull,default:current_timestamp" json:"createdAt"`
    UpdatedAt   time.Time  `bun:"updated_at,nullzero,notnull,default:current_timestamp" json:"updatedAt"`
}
```

**Indexes:** unique on `(user_id, lower(name))` (partial: `WHERE deleted_at IS NULL`). Unique partial on `public_slug WHERE is_public = true`.
**FK:** `user_id` → `users(id)` `ON DELETE CASCADE`.

### 7.8 `list_items` (M:N with order)

```go
type ListItem struct {
    bun.BaseModel `bun:"table:list_items,alias:li"`

    ListID    int64     `bun:"list_id,pk"  json:"listId"`
    ItemID    int64     `bun:"item_id,pk"  json:"itemId"`
    Position  int       `bun:"position,notnull,default:0" json:"position"`
    AddedAt   time.Time `bun:"added_at,nullzero,notnull,default:current_timestamp" json:"addedAt"`
}
```

**Primary key:** composite `(list_id, item_id)`.
**Indexes:** `(list_id, position)` for ordered listing. `(item_id)` for "which lists is this in".
**FK:** `list_id` → `lists(id)` `ON DELETE CASCADE`; `item_id` → `items(id)` `ON DELETE CASCADE`.

**Reordering:** `position` is a per-list integer. New items insert at `max(position) + 1`. Reorders rewrite the affected slice; v1 does **not** use fractional indexing (overkill for expected list sizes).

### 7.9 `tags`

```go
type Tag struct {
    bun.BaseModel `bun:"table:tags,alias:tags"`

    ID        int64     `bun:"id,pk,autoincrement"  json:"id"`
    UserID    int64     `bun:"user_id,notnull"      json:"userId"`
    Name      string    `bun:"name,notnull"         json:"name"`         // display
    Slug      string    `bun:"slug,notnull"         json:"slug"`         // normalised: lowercase, dash-separated
    CreatedAt time.Time `bun:"created_at,nullzero,notnull,default:current_timestamp" json:"createdAt"`
}
```

**Indexes:** unique on `(user_id, slug)`.
**FK:** `user_id` → `users(id)` `ON DELETE CASCADE`.

**Normalisation:** `slug = strings.ToLower(strings.Join(strings.Fields(strings.Map(toAlphaNumOrSpace, name)), "-"))`. Tags upserted by slug; original casing preserved in `name` (first writer wins).

### 7.10 `item_tags`

```go
type ItemTag struct {
    bun.BaseModel `bun:"table:item_tags,alias:it"`

    ItemID    int64     `bun:"item_id,pk"           json:"itemId"`
    TagID     int64     `bun:"tag_id,pk"            json:"tagId"`
    Source    string    `bun:"source,notnull,default:'user'" json:"source"` // "user" | "ai"
    CreatedAt time.Time `bun:"created_at,nullzero,notnull,default:current_timestamp" json:"createdAt"`
}
```

**PK:** `(item_id, tag_id)`.
**CHECK:** `source IN ('user','ai')`.
**Indexes:** `(tag_id, item_id)` for "items with this tag".
**FK:** cascading both ways.

### 7.11 `highlights`

```go
type Highlight struct {
    bun.BaseModel `bun:"table:highlights,alias:hl"`

    ID        int64     `bun:"id,pk,autoincrement"   json:"id"`
    ItemID    int64     `bun:"item_id,notnull"       json:"itemId"`
    Text      string    `bun:"text,notnull"          json:"text"`
    Note      string    `bun:"note"                  json:"note,omitempty"`
    Color     string    `bun:"color"                 json:"color,omitempty"`     // "yellow" | "green" | "blue" | "red" | "purple"
    Context   string    `bun:"context"               json:"context,omitempty"`   // chars around the highlight for re-anchoring
    Position  int       `bun:"position,default:0"    json:"position"`            // ordering hint within an item
    CreatedAt time.Time `bun:"created_at,nullzero,notnull,default:current_timestamp" json:"createdAt"`
    UpdatedAt time.Time `bun:"updated_at,nullzero,notnull,default:current_timestamp" json:"updatedAt"`
}
```

**FK:** `item_id` → `items(id)` `ON DELETE CASCADE`.
**Notes:** v1 stores textual highlights only — no DOM-anchor JSON, no PDF page rectangles. v2 may add a `position_data jsonb` column.

### 7.12 `jobs`

```go
type Job struct {
    bun.BaseModel `bun:"table:jobs,alias:jobs"`

    ID          int64     `bun:"id,pk,autoincrement"          json:"id"`
    Type        string    `bun:"type,notnull"                 json:"type"`
    Payload     []byte    `bun:"payload,type:jsonb,notnull"   json:"-"`
    State       string    `bun:"state,notnull,default:'queued'" json:"state"`     // queued | running | succeeded | failed | dead
    Attempts    int       `bun:"attempts,notnull,default:0"   json:"attempts"`
    MaxAttempts int       `bun:"max_attempts,notnull,default:5" json:"maxAttempts"`
    RunAt       time.Time `bun:"run_at,notnull,default:current_timestamp" json:"runAt"`
    LockedAt    *time.Time `bun:"locked_at,nullzero"          json:"lockedAt,omitempty"`
    LockedBy    string    `bun:"locked_by"                    json:"lockedBy,omitempty"` // worker id
    LastError   string    `bun:"last_error"                   json:"lastError,omitempty"`
    CreatedAt   time.Time `bun:"created_at,nullzero,notnull,default:current_timestamp" json:"createdAt"`
    UpdatedAt   time.Time `bun:"updated_at,nullzero,notnull,default:current_timestamp" json:"updatedAt"`
}
```

**CHECK:** `state IN ('queued','running','succeeded','failed','dead')`.
**Indexes:**

- `(state, run_at)` for the worker poll.
- `(type, state)` for monitoring.
- Partial GIN on `payload` only if we later need to query by payload contents (defer).

**Pruning:** `succeeded` rows older than 7 days are deleted by the `purge_trash` job (which also handles other housekeeping).

### 7.13 Foreign keys migration

A single final migration adds every FK constraint named explicitly, mirroring `schoolbell/db/migrations/20260509110801_add_foreign_keys.go`:

```
fk_sessions_user             sessions.user_id              → users.id            ON DELETE CASCADE
fk_password_reset_tokens_user password_reset_tokens.user_id → users.id           ON DELETE CASCADE
fk_items_user                items.user_id                 → users.id            ON DELETE CASCADE
fk_bookmark_details_item     bookmark_details.item_id      → items.id            ON DELETE CASCADE
fk_bookmark_details_user     bookmark_details.user_id      → users.id            ON DELETE CASCADE
fk_bookmark_details_thumb    bookmark_details.thumbnail_file_id → file_details.item_id ON DELETE SET NULL
fk_note_details_item         note_details.item_id          → items.id            ON DELETE CASCADE
fk_file_details_item         file_details.item_id          → items.id            ON DELETE CASCADE
fk_lists_user                lists.user_id                 → users.id            ON DELETE CASCADE
fk_list_items_list           list_items.list_id            → lists.id            ON DELETE CASCADE
fk_list_items_item           list_items.item_id            → items.id            ON DELETE CASCADE
fk_tags_user                 tags.user_id                  → users.id            ON DELETE CASCADE
fk_item_tags_item            item_tags.item_id             → items.id            ON DELETE CASCADE
fk_item_tags_tag             item_tags.tag_id              → tags.id             ON DELETE CASCADE
fk_highlights_item           highlights.item_id            → items.id            ON DELETE CASCADE
```

### 7.14 ER diagram (textual)

```
users 1 ── n sessions
users 1 ── n password_reset_tokens
users 1 ── n items
users 1 ── n lists
users 1 ── n tags

items 1 ── 0..1 bookmark_details
items 1 ── 0..1 note_details
items 1 ── 0..1 file_details
items 1 ── n highlights
items n ── n lists       (via list_items)
items n ── n tags        (via item_tags)
file_details 1 ── n bookmark_details (as thumbnail)   [optional FK]

jobs                                                         (standalone)
```

---

## 8. Content Pipelines

This section describes what happens when each kind of content is created.

### 8.1 Bookmark (link)

`POST /api/v1/bookmarks` with `{ url, listId?, title?, note?, tags? }`.

1. Controller binds + validates (Zog: URL parseable, not in private CIDR — see §19.7).
2. Service:
    1. Normalise URL (lowercase scheme/host, strip default ports, strip `utm_*` and `fbclid`, sort query keys). Compute `url_hash = sha256(normalised)`.
    2. Check `(user_id, url_hash)` — if exists, return 409 Conflict with the existing item's id. Frontend can then offer "open existing" vs "create anyway with `?force=true`".
    3. In one transaction: insert `items` (`type='bookmark'`, `state='pending'`, `title = title || hostname(url)`), insert `bookmark_details` (with URL + URLHash + Domain), attach tags if provided, enqueue jobs:
        - `fetch_link_metadata { item_id }`
        - `llm_tag_summarize { item_id }` (gated on `llm_enabled`)
4. Return 201 with the item (state = `pending`).

**`fetch_link_metadata` job (in `app/jobs/fetch_link_metadata.go`):**

1. `httpfetch.Get(url)` — see §19.7 for the safe fetcher (timeouts, redirect cap, max bytes, deny private IPs).
2. Parse HTML for `<title>`, OpenGraph (`og:title`, `og:description`, `og:image`, `og:site_name`, `og:author`), `<meta name="description">`, favicon (`<link rel="icon">`).
3. Update `bookmark_details` with discovered fields and `items.title`/`items.summary` (if they were empty).
4. If `og:image` exists and is < 2 MiB, enqueue `generate_thumbnail { item_id, image_url }`.
5. On success, leave item state alone (it might still be `pending` for LLM); on permanent failure (4xx/5xx/timeout after retries), set `items.state='error'` and record `last_error` on the job row.

**`generate_thumbnail` job:** download image (with same safe fetcher), produce a max-512×512 JPEG, store via `Storage.Put`, insert `file_details` row of `mime_type='image/jpeg'`, link `bookmark_details.thumbnail_file_id`.

**`llm_tag_summarize` job:** described in §11.

### 8.2 Note (plain) / markdown

`POST /api/v1/notes` with `{ title?, body, format }` where `format ∈ {plain, markdown}`.

1. Validate (Zog: body non-empty, format in enum).
2. Insert `items` (`type=format=='markdown' ? 'markdown' : 'note'`, `state='pending'` if `llm_enabled` else `'ready'`), insert `note_details`.
3. If `llm_enabled` and body length ≥ `llm_min_chars` (say 200), enqueue `llm_tag_summarize { item_id }`.
4. Return 201.

### 8.3 File (image / PDF)

`POST /api/v1/files` multipart (`file` field, optional `title`, `listId`, `tags`).

1. Reject if size > `storage_max_upload_mb`.
2. Sniff MIME from content (`http.DetectContentType` on first 512 bytes). Validate against allow-list: `image/jpeg`, `image/png`, `image/webp`, `image/gif`, `application/pdf`.
3. Compute sha256 streamed. Generate `storage_key = "<user_id>/<yyyy>/<mm>/<uuid>"`.
4. `Storage.Put(storage_key, body, mime_type)`.
5. In one transaction: insert `items` (`type ∈ {image, pdf}`, `state='pending'`), insert `file_details`.
6. For images: enqueue `generate_thumbnail { item_id }` (re-encode-and-resize as JPEG, max 1024×1024 for preview, plus a 256×256 thumb).
7. For PDFs: enqueue `llm_tag_summarize { item_id }` with `text` derived from the first N pages — **v1 does not run OCR**; if a PDF has no embedded text, we send an empty document to the LLM and the LLM call is skipped server-side (no-op tag).
8. Return 201.

### 8.4 State transitions

```
   pending ──(metadata + LLM done)──▶ ready
   pending ──(permanent fetch error)─▶ error
   ready/error ──(user re-trigger)───▶ pending
```

The frontend polls `GET /items/:id` to see state transitions. No WebSocket in v1.

---

## 9. Lists, Tags, Highlights

### 9.1 Lists

- Per-user; M:N to items via `list_items`.
- Order is per-list (`list_items.position`, integer); reorder rewrites positions of affected items.
- A list with no items is valid.
- Lists can be public (§16). A public list returns its public items only; private items in a public list are hidden.
- Lists are soft-deletable. Deleting a list does not delete its items; restoring brings the list back with the same `list_items` rows.

### 9.2 Inbox

The "Inbox" is a virtual list, not a row in `lists`. It is the result of:

```sql
SELECT * FROM items i
WHERE i.user_id = ?
  AND i.deleted_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM list_items li WHERE li.item_id = i.id)
ORDER BY i.pinned DESC, i.created_at DESC;
```

Exposed at `GET /api/v1/lists/inbox`.

### 9.3 Tags

- Per-user namespace.
- Slug-normalised (§7.9). On `POST /items/:id/tags` with `{ name }`, the service upserts a `tags` row by `(user_id, slug)` and inserts an `item_tags` row.
- `source` distinguishes user vs AI. The UI can show AI-suggested tags differently.
- Renaming a tag (`PATCH /tags/:id`) re-computes `slug` and may merge with an existing tag if the new slug already exists for the user — merging consolidates `item_tags` rows.
- Deleting a tag (`DELETE /tags/:id`) cascades to `item_tags`.

### 9.4 Highlights

- Per-item. Plain text + optional note + color.
- `position` is a stable ordering hint (e.g. character offset within `note_details.body`, or just insertion order for bookmarks/files). v1 keeps it loose; v2 may formalise anchoring.

---

## 10. Search

### 10.1 What we index

For each item, the following is concatenated into `items.search_text` whenever the item is created or updated:

- `items.title`
- `items.summary`
- `items.ai_category`
- All attached tag names
- Type-specific:
  - bookmark: `bookmark_details.url`, `description`, `site_name`, `author`
  - note/markdown: `note_details.body` (truncated to 32 KiB)
  - file: `file_details.original_filename` (and, in future, OCR-extracted text)

`search_text` is denormalised. Services that mutate any of the above must call `items.RefreshSearchText(itemID)` in the same transaction.

### 10.2 `tsvector` column

```sql
ALTER TABLE items
  ADD COLUMN search_tsv tsvector
  GENERATED ALWAYS AS (to_tsvector('simple', coalesce(search_text, ''))) STORED;

CREATE INDEX items_search_tsv_gin ON items USING gin (search_tsv);
```

We use the `'simple'` configuration to avoid English-only stemming; this makes search behave predictably for tags, URLs, code fragments, and non-English content. v2 can switch to per-user language config.

### 10.3 Trigram index for fuzzy title

```sql
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE INDEX items_title_trgm ON items USING gin (title gin_trgm_ops);
```

Used for typo-tolerant title matching when the FTS query produces no results.

### 10.4 Query

`GET /api/v1/search?q=<terms>&type=&tag=&list_id=&from=&to=&pinned=&is_public=&cursor=&limit=`

```sql
SELECT i.*,
       ts_rank(i.search_tsv, plainto_tsquery('simple', $1)) AS rank,
       greatest(similarity(i.title, $1), 0) AS title_sim
FROM items i
WHERE i.user_id = $user
  AND i.deleted_at IS NULL
  AND ($1 = '' OR i.search_tsv @@ plainto_tsquery('simple', $1)
                OR i.title % $1)                                       -- trigram fallback
  AND ($type IS NULL OR i.type = $type)
  AND ($list_id IS NULL OR EXISTS (
        SELECT 1 FROM list_items li WHERE li.item_id = i.id AND li.list_id = $list_id))
  AND ($tag_id IS NULL OR EXISTS (
        SELECT 1 FROM item_tags it WHERE it.item_id = i.id AND it.tag_id = $tag_id))
  AND ($from IS NULL OR i.created_at >= $from)
  AND ($to   IS NULL OR i.created_at <= $to)
  AND ($pinned IS NULL OR i.pinned = $pinned)
  AND ($is_public IS NULL OR i.is_public = $is_public)
ORDER BY i.pinned DESC,
         rank DESC,
         title_sim DESC,
         i.created_at DESC
LIMIT $limit + 1;
```

`limit + 1` is the standard cursor-pagination trick: we return `limit` items and a `nextCursor` if there's a `+1th`.

---

## 11. LLM Integration

### 11.1 Provider interface

```go
package llm

type Provider interface {
    // Suggest returns proposed tags, a short summary, and a category label for
    // the given content. The implementation MAY truncate `text` to a model-safe
    // length and SHOULD return promptly or honour ctx cancellation.
    Suggest(ctx context.Context, in SuggestInput) (SuggestOutput, error)
}

type SuggestInput struct {
    Kind  string   // "bookmark" | "note" | "markdown" | "image" | "pdf"
    Title string
    Text  string   // raw content or extracted text
    URL   string   // empty if not a bookmark
}

type SuggestOutput struct {
    Tags     []string // lowercase, dash-separated slugs preferred
    Summary  string   // 1–3 sentences
    Category string   // single short label, e.g. "github-repo", "recipe"
}
```

v1 has two adapters:

- `openai.NewProvider(cfg)` — POSTs to `{base_url}/chat/completions` with the configured model, using JSON-mode (`response_format: { type: "json_object" }`) and a system prompt that defines the required JSON schema.
- `ollama.NewProvider(cfg)` — POSTs to `{base_url}/api/chat` with the configured model, asking the model to reply with strict JSON (Ollama supports the `format: "json"` option).

Both adapters share a `buildPrompt(in)` helper.

### 11.2 Prompt outline

```
SYSTEM
You are an assistant that classifies and summarises content saved by a user
to a personal bookmark app called Kluttr. You MUST respond with a single JSON
object matching this schema:

{
  "tags": [string, ...],     // 3-7 short, lowercase, kebab-case topic tags
  "summary": string,         // 1-3 sentences, neutral tone, no marketing
  "category": string         // single short label
}

USER
Kind: {{ .Kind }}
Title: {{ .Title }}
URL: {{ .URL }}
Content:
{{ .Text | truncate llm_max_input_chars }}
```

### 11.3 `llm_tag_summarize` job

1. Skip if `llm_enabled = false`.
2. Load item + appropriate detail. Build `SuggestInput`.
3. Call `Provider.Suggest(ctx, in)` with `ctx` having `llm_request_timeout_seconds`.
4. On success:
    - Upsert returned tags (slug-normalised) with `source='ai'`.
    - Update `items.summary` and `items.ai_category` (only if not already user-set).
    - Refresh `items.search_text`.
    - Transition `items.state` to `ready`.
5. On failure: increment job attempts; on `max_attempts` exceeded, transition `items.state` to `ready` anyway (the item is usable without AI metadata) and record the error.

### 11.4 Manual re-runs

`POST /items/:id/actions/retag` enqueues a fresh `llm_tag_summarize`. The frontend exposes this so users can re-run AI on items where the model produced poor output, or after switching providers.

### 11.5 Token / cost safety

- `llm_max_input_chars` caps input size (default ~12 KiB ≈ a few thousand tokens).
- A single global concurrency limit (≤ `worker_concurrency`) bounds outbound LLM calls.
- Per-user throttling is **not** added in v1; the job queue is fair (FIFO within type), and self-hosted instances don't need it.

---

## 12. File Storage

### 12.1 Interface

```go
package storage

type Storage interface {
    Put(ctx context.Context, key string, body io.Reader, mimeType string, size int64) error
    Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error)
    Delete(ctx context.Context, key string) error
    SignedURL(ctx context.Context, key string, ttl time.Duration) (string, error) // empty string ⇒ "no signed-URL support"
}

type ObjectInfo struct {
    Size        int64
    MimeType    string
    LastModified time.Time
    ETag         string
}
```

### 12.2 Adapters

**`local`** (`app/storage/local.go`)

- Root path: `storage_local_path` (e.g. `./data/storage`).
- Key → path: `root/key` (key already contains slashes; we ensure no `..`).
- `SignedURL` returns `""` — the controller falls back to streaming through `GET /files/:id`.

**`s3`** (`app/storage/s3.go`)

- Endpoint, region, bucket, prefix, credentials from config. `force_path_style` for MinIO.
- `SignedURL` returns a presigned GET URL with `ttl` = `storage_signed_url_ttl_seconds`.

### 12.3 Serving files

`GET /api/v1/files/:id`:

1. Auth required; verify `items.user_id == current_user.id` **or** `items.is_public`.
2. Resolve `file_details.storage_key`.
3. If backend returns a signed URL, redirect `302` to it.
4. Otherwise, stream through with `Content-Type`, `Content-Length`, `Content-Disposition: inline; filename="..."`, `Cache-Control: private, max-age=300`.

Public access goes through `/p/:slug` (§16), not `/files/:id`.

### 12.4 Upload limits and safety

- Max body bytes enforced at the Raptor server level (`server.max_body_bytes`) and re-checked in the controller.
- MIME sniffed from content, not trusted from the client header.
- Image dimensions read with `image.DecodeConfig` before full decode to defend against decompression bombs (reject if > 12000×12000).
- File names sanitised (`filepath.Base`, strip control chars) for `Content-Disposition`.

---

## 13. Background Jobs

### 13.1 Schema

See §7.12.

### 13.2 Enqueue API

```go
// Inside a service method, within the same Bun transaction as the originating write:
err := jobs.Enqueue(ctx, tx, jobs.FetchLinkMetadata{ItemID: item.ID})
```

`Enqueue` marshals the typed payload to JSON, derives `type` from the Go type's `JobType()` method, sets `run_at = now()` (or a future time for delayed jobs), and inserts a `jobs` row inside `tx`.

### 13.3 Worker loop

`JobsService.Setup()` starts `worker_concurrency` goroutines and one supervisor goroutine. Each worker:

```
for {
    select {
    case <-ctx.Done(): return
    default:
    }

    err := tx(func() {
        // SELECT 1 job WHERE state = 'queued' AND run_at <= now()
        // ORDER BY run_at FOR UPDATE SKIP LOCKED LIMIT 1
        // UPDATE state = 'running', locked_at = now(), locked_by = workerID, attempts = attempts + 1
        // RETURNING *
    })
    if no job: sleep(worker_poll_interval_ms); continue
    if err: log, sleep, continue

    handler := registry[job.type]
    err := handler.Handle(ctx, job.payload)
    if err == nil {
        UPDATE jobs SET state='succeeded' WHERE id = ?
    } else if job.attempts >= job.max_attempts {
        UPDATE jobs SET state='dead', last_error=err.Error() WHERE id = ?
    } else {
        // exponential backoff: run_at = now() + min(2^attempts seconds, 1 hour)
        UPDATE jobs SET state='queued', run_at = ?, last_error=err.Error() WHERE id = ?
    }
}
```

### 13.4 Registered job types

| Type | Payload | Triggered by |
|---|---|---|
| `fetch_link_metadata` | `{ item_id }` | bookmark create / `actions/refetch` |
| `generate_thumbnail` | `{ item_id, source }` (source = `"upload"` \| `"url"`, with URL if remote) | bookmark metadata done / image upload |
| `llm_tag_summarize` | `{ item_id }` | item create / `actions/retag` |
| `purge_trash` | `{}` | scheduled hourly |
| `cleanup_expired_sessions` | `{}` | scheduled hourly |

### 13.5 Scheduling

Scheduled jobs aren't a separate scheduler component. On boot, `JobsService.Setup()` upserts a "singleton" queued job for each scheduled type if no `queued`/`running` row exists. After the handler runs, it enqueues the next occurrence with `run_at = now() + interval`. This keeps everything in one mechanism.

### 13.6 Swap-out plan (future)

If we outgrow in-process Postgres workers (unlikely for a self-hosted app), the `jobs` package boundary lets us swap to River, asynq, NATS, etc. without touching service code — services only call `jobs.Enqueue(ctx, tx, payload)`.

---

## 14. API Conventions

### 14.1 Base URL and versioning

All endpoints are mounted under `/api/v1`. The version segment in the path is the *only* versioning mechanism. v2 ships side-by-side at `/api/v2`.

### 14.2 Media types

- Requests and responses are **JSON only** (UTF-8). The only exception is `POST /files` (multipart) and `GET /files/:id` (binary streaming).
- All JSON property names are **camelCase**. Bun column names are snake_case; the `json:` struct tag handles translation.
- Timestamps are **ISO 8601 with timezone**, e.g. `"2026-05-16T12:34:56Z"`.

### 14.3 Authentication

`Authorization: Bearer <token>` on every route except the §6.7 exclusion list. Missing/invalid → `401 Unauthorized`.

### 14.4 Errors

Errors are JSON objects with a stable shape:

```json
{
  "error": {
    "status": 422,
    "code": "validation_failed",
    "message": "Invalid request body",
    "details": [
      { "field": "url", "code": "invalid_url", "message": "must be a valid http(s) URL" }
    ]
  }
}
```

Codes used:

| HTTP | code |
|---|---|
| 400 | `bad_request` |
| 401 | `unauthorized` |
| 403 | `forbidden` |
| 404 | `not_found` |
| 409 | `conflict` (also: `duplicate_url` with `existingItemId` in details) |
| 422 | `validation_failed` |
| 429 | `rate_limited` |
| 500 | `internal` |

Raptor's `errs.NewError*` is used to construct typed errors. The middleware translates Postgres errors via `DatabaseService.HandleError` (mirrors `schoolbell`).

### 14.5 Pagination

**Cursor pagination** for item lists, search, lists of lists, tags:

Request: `?cursor=<opaque>&limit=<n>` (default 25, max 100).

Response wrapper:

```json
{
  "data": [ ... ],
  "pageInfo": {
    "limit": 25,
    "nextCursor": "eyJpZCI6MTIzLCJjcmVhdGVkQXQiOiIuLi4ifQ==",
    "hasMore": true
  }
}
```

`cursor` is a base64-encoded JSON `{ id, createdAt }` (or analogous keys for the sort in use). Cursors are opaque to the client.

### 14.6 Sorting and filtering

- Sort: `?sort=-created_at,title` (prefix `-` = desc). The set of allowed sort fields is documented per endpoint.
- Filter: query params per endpoint. Multiple values via repeated param (`?tag=foo&tag=bar`) or comma-separated (`?tag=foo,bar`) — endpoints document which.

### 14.7 Request validation

Every controller binds the JSON body into a `<Resource>Request` struct (declared in `app/models/<name>.go` next to the Bun struct), then passes it through the corresponding Zog schema from `ValidationService`. Zog errors map to `422 validation_failed` with field-level `details`.

### 14.8 Idempotency

Write endpoints accept an optional `Idempotency-Key` header. If present, the response is cached in Ristretto for 24h keyed by `(user_id, route, idempotency_key)`; replays return the cached response. v1 implements this only for `POST /bookmarks`, `POST /notes`, `POST /files`.

### 14.9 Rate limiting

| Scope | Limit | Behaviour |
|---|---|---|
| `/auth/*` (per IP) | `rate_limit_auth_per_ip` / min | 429 with `Retry-After` |
| Authenticated API (per user) | `rate_limit_api_per_user` / min | 429 with `Retry-After` |
| `/p/*` (per IP) | `rate_limit_public_per_ip` / min | 429 with `Retry-After` |

In-process token-bucket limiter, single instance, in-memory.

### 14.10 CORS

Configured by `cors_allow_origins` (comma-separated). Standard headers: `Authorization, Content-Type, Idempotency-Key, X-Request-Id`. `Access-Control-Allow-Credentials: false` (we use Bearer, not cookies).

---

## 15. Endpoint Catalog

Each endpoint lists: method, path, auth, request shape, response shape, key errors. Shapes use TypeScript-ish syntax for brevity; the canonical Go types live in `app/models/`.

### 15.1 Auth

#### `POST /api/v1/auth/register` (no auth)

Request:
```ts
{ email: string, password: string, displayName?: string }
```

Response 201:
```ts
{ token: string, user: UserResponse }
```

Errors: `409 conflict` (email exists), `422 validation_failed`, `403 forbidden` (when `signup_enabled = false`).

#### `POST /api/v1/auth/login` (no auth)

Request: `{ email: string, password: string }`
Response 200: `{ token: string, user: UserResponse }`
Errors: `401 unauthorized` (invalid credentials), `429 rate_limited`.

#### `POST /api/v1/auth/logout`

Response 204.

#### `GET /api/v1/auth/me`

Response 200: `UserResponse`.

#### `POST /api/v1/auth/password`

Request: `{ currentPassword: string, newPassword: string }`
Response 204. Invalidates all sessions for this user.

#### `POST /api/v1/auth/password/reset` (no auth)

Request: `{ email: string }`
Response 204 (always — does not reveal whether the email exists).
Side-effect: if email exists and SMTP enabled, an email containing a one-shot reset URL is sent.

#### `POST /api/v1/auth/password/reset/confirm` (no auth)

Request: `{ token: string, newPassword: string }`
Response 204.
Errors: `400 bad_request` (token unknown/expired/used).

### 15.2 Items (generic)

#### `GET /api/v1/items`

Query: `type, listId, tagId, q, pinned, state, deleted, from, to, sort, cursor, limit`.
- `deleted` ∈ `false | true | only` — `false` (default) hides trash; `only` shows trash exclusively; `true` includes both.
- `q` does a basic title-substring match; for full search use `/search`.

Response 200: paginated `Item[]` (with included tags; details lazy — set `include=detail` to inline the type-specific detail).

#### `GET /api/v1/items/:id`

Response 200: full `Item` including `bookmark`/`note`/`file` detail and `tags[]`, `lists[]`, `highlightsCount`.

#### `PATCH /api/v1/items/:id`

Request (any subset): `{ title?, summary?, aiCategory?, pinned?, isPublic? }`.
Returns the updated item. Setting `isPublic: true` allocates a `publicSlug` if none.

#### `DELETE /api/v1/items/:id`

Soft-deletes. Response 204.

#### `POST /api/v1/items/:id/restore`

Response 200: restored item.

#### `DELETE /api/v1/items/:id/purge`

Hard-deletes (item + details + tag/list links + highlights + storage objects). Response 204.

#### `POST /api/v1/items/bulk`

Request:
```ts
{
  ids: number[],            // required, max 200
  action: "delete" | "restore" | "purge" | "tag" | "untag"
        | "move_to_list" | "remove_from_list" | "pin" | "unpin"
        | "make_public" | "make_private",
  params?: {
    tagIds?: number[],
    listId?: number,
  }
}
```
Response 200: `{ updated: number, skipped: number, errors: { id, code }[] }`.

### 15.3 Bookmarks / Notes / Files (creators)

#### `POST /api/v1/bookmarks`

Request:
```ts
{
  url: string,
  title?: string,
  summary?: string,
  listId?: number,
  tags?: string[],          // tag names
  force?: boolean           // bypass duplicate check
}
```
Response 201: `Item` (with bookmark detail). The item is in state `pending` until the metadata+LLM jobs complete.
Errors: `409 conflict` with `details: { existingItemId, url }` on duplicate.

#### `POST /api/v1/notes`

Request:
```ts
{
  title?: string,
  body: string,
  format: "plain" | "markdown",
  listId?: number,
  tags?: string[]
}
```
Response 201: `Item` (with note detail).

#### `POST /api/v1/files` (multipart)

Form fields: `file` (binary, required), `title?`, `listId?`, `tags?` (comma-separated).
Response 201: `Item` (with file detail). For images, a thumbnail will be ready after the `generate_thumbnail` job runs (frontend polls).

#### `GET /api/v1/files/:id`

Streams or 302-redirects (see §12.3). `?download=1` adds `Content-Disposition: attachment`.

#### `GET /api/v1/files/:id/thumbnail`

Returns the thumbnail (or 404 if not yet generated).

### 15.4 Lists

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/v1/lists` | Paginated; query `?include=counts` adds `itemsCount`. |
| `POST` | `/api/v1/lists` | Body: `{ name, description?, icon?, color?, isPublic? }`. |
| `GET` | `/api/v1/lists/inbox` | Virtual list — items with no list membership. |
| `GET` | `/api/v1/lists/:id` | Detail with first-page items inlined. |
| `PATCH` | `/api/v1/lists/:id` | Update name/description/icon/color/isPublic. Toggling public allocates slug. |
| `DELETE` | `/api/v1/lists/:id` | Soft-delete. |
| `POST` | `/api/v1/lists/:id/restore` | |
| `DELETE` | `/api/v1/lists/:id/purge` | Hard-delete (items remain). |
| `GET` | `/api/v1/lists/:id/items` | Paginated; sorted by `position`. |
| `POST` | `/api/v1/lists/:id/items` | Body: `{ itemId, position? }`. Defaults to end. |
| `PATCH` | `/api/v1/lists/:id/items/:itemId` | Body: `{ position }`. Triggers reorder write. |
| `DELETE` | `/api/v1/lists/:id/items/:itemId` | Removes link, does not delete item. |

### 15.5 Tags

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/v1/tags` | `?q=` substring; `?source=user|ai|all`; `?include=counts`. |
| `PATCH` | `/api/v1/tags/:id` | Body: `{ name }`. May merge if slug collision. |
| `DELETE` | `/api/v1/tags/:id` | Detaches from all items. |
| `POST` | `/api/v1/items/:id/tags` | Body: `{ names: string[] }`; upserts. |
| `DELETE` | `/api/v1/items/:id/tags/:tagId` | Detach one. |

### 15.6 Highlights

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/v1/highlights` | Required `?itemId=`. |
| `POST` | `/api/v1/highlights` | Body: `{ itemId, text, note?, color?, position? }`. |
| `PATCH` | `/api/v1/highlights/:id` | Body: subset. |
| `DELETE` | `/api/v1/highlights/:id` | |

### 15.7 Search

#### `GET /api/v1/search`

Query: `q` (required, may be empty for filter-only browsing), `type`, `tagId`, `listId`, `from`, `to`, `pinned`, `cursor`, `limit`.
Response 200: paginated list of items with `rank` and `snippet` (server-rendered FTS snippet, max 240 chars).

### 15.8 Actions

| Method | Path | Notes |
|---|---|---|
| `POST` | `/api/v1/items/:id/actions/refetch` | Re-fetch link metadata (bookmark only). 202 Accepted. |
| `POST` | `/api/v1/items/:id/actions/retag` | Re-run LLM. 202 Accepted. |
| `POST` | `/api/v1/items/:id/actions/resummarize` | Same as `retag` but only refreshes `summary`/`aiCategory`. 202. |

### 15.9 Public

#### `GET /p/:slug` (no auth)

Returns a read-only JSON representation of the item or list. Rate-limited per IP (§14.9). See §16.

### 15.10 System

| Method | Path | Notes |
|---|---|---|
| `GET` | `/healthz` | Liveness — always returns 200 if process is up. |
| `GET` | `/readyz` | DB ping + storage `Get(<probe key>)`. 503 on failure. |
| `GET` | `/version` | `{ version, commit, buildDate, goVersion }`. |

> **Note:** the system endpoints are mounted **outside** `/api/v1` to match standard ops practice. Routes definition in `config/routes.yaml`.

---

## 16. Public Sharing

### 16.1 Model

A user toggles `isPublic` on an item or a list. The server allocates an unguessable slug if one is not already set, and clears it on a private→public→private cycle only if the user opts to "rotate slug". Otherwise the slug is reused so the link stays valid across visibility toggles.

### 16.2 Slug generation

```go
func NewPublicSlug() string {
    var b [9]byte                 // 9 bytes → 12 base62 chars
    _, _ = rand.Read(b[:])
    return base62Encode(b[:])     // ~6e15 keyspace; brute force not viable with rate limit
}
```

### 16.3 Public payload

`GET /p/:slug` returns:

For an item (`type=bookmark`):
```ts
{
  type: "bookmark",
  title, summary, aiCategory, createdAt, updatedAt,
  bookmark: { url, domain, siteName, author, description, imageUrl, faviconUrl, publishedAt },
  tags: [{ name, slug }],
  owner: { displayName }    // never email
}
```

For a list:
```ts
{
  type: "list",
  name, description, icon, color, createdAt,
  items: PublicItem[],      // public items only, paginated
  owner: { displayName }
}
```

For an item with files: a public, time-limited URL to the file content (signed if S3, proxied through `/p/files/:slug/:fileSlug` if local).

### 16.4 Headers

```
Cache-Control: public, max-age=60, stale-while-revalidate=300
X-Robots-Tag: noindex, nofollow    # default; user may opt-in to indexing in v2
```

### 16.5 Disabling

Toggling `isPublic = false` immediately makes `/p/:slug` return `404 not_found`. The slug remains in the DB for re-enable continuity unless rotated.

---

## 17. Soft-Delete & Trash

- `items.deleted_at` and `lists.deleted_at` are nullable.
- `Bun`'s `,soft_delete` tag makes default selects exclude deleted rows.
- Trash views:
  - `GET /items?deleted=only` — items in trash.
  - `GET /lists?deleted=only` — lists in trash.
- Restore (`POST /items/:id/restore`) clears `deleted_at`.
- Purge (`DELETE /items/:id/purge`):
  - Hard-deletes `items` row (cascades to details, list_items, item_tags, highlights via FK).
  - For files: also deletes the storage object(s) — call `Storage.Delete(storageKey)` and `Delete(thumbnailKey)`.
- `purge_trash` scheduled job runs hourly, purging items with `deleted_at < now() - trash_purge_days`.
- Bulk purge / restore via `POST /items/bulk`.

---

## 18. Observability

### 18.1 Logging

- `slog` JSON in production (no tint), tint in dev.
- Every request log line: `request_id` (random 16 hex), `user_id` (if auth'd), `method`, `path`, `status`, `latency_ms`, `bytes_out`.
- Service logs include `request_id` via context propagation.
- Job logs include `job_id`, `job_type`, `attempt`, `latency_ms`, `error` if any.

### 18.2 Health endpoints

- `GET /healthz` — 200 OK, no body. For container liveness probes.
- `GET /readyz` — 200 OK if DB query succeeds and storage probe succeeds; else 503 with a JSON body listing failed components.
- `GET /version` — build info baked in via `-ldflags "-X main.version=... -X main.commit=..."`.

### 18.3 Metrics (stub)

No `/metrics` endpoint in v1. Code includes hooks (counter/histogram interfaces in `app/services/metrics_service.go` returning no-ops) so a Prometheus exporter can be dropped in for v2 without touching call sites.

---

## 19. Security Checklist

### 19.1 Passwords

Argon2id with parameters in §6.4. Verification recomputes from encoded string. Optional bcrypt fallback documented.

### 19.2 Tokens

32 bytes from `crypto/rand`, sha256-hashed at rest. No JWT. Tokens are not logged anywhere. Token strings are not included in any audit fields.

### 19.3 CORS / CSRF

CORS restricted to `cors_allow_origins`. We use Bearer tokens, not cookies — there is no CSRF risk against the API. The `/p/:slug` endpoint is GET-only, no state changes.

### 19.4 Rate limits

Per §14.9. Hard caps on bulk endpoints (`POST /items/bulk` capped at 200 ids per call).

### 19.5 Input validation

Every request body passes through Zog. Critical invariants enforced:

- URLs are RFC 3986-parseable and scheme ∈ `{http, https}`.
- Tag names: 1–64 characters; control chars stripped.
- Display names: 1–64 characters.
- Bulk action `ids` ≤ 200; tag/list ids exist and belong to the caller.

### 19.6 File-upload safety

- Max size from config.
- MIME sniffing (`http.DetectContentType`) — header is ignored.
- Image dimensions pre-checked via `image.DecodeConfig` to defend against decompression bombs.
- PDFs: structural validation via header magic (`%PDF-`).
- Storage keys never include user-supplied paths; always server-generated.

### 19.7 SSRF protection on URL fetches

`app/utils/httpfetch.go` is the *only* place that does outbound HTTP for user-supplied URLs (link metadata fetch, remote thumbnail download). It enforces:

- Scheme ∈ `{http, https}`.
- DNS lookup before connect; reject if any resolved IP is in:
  - RFC 1918 private ranges (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`).
  - Link-local (`169.254.0.0/16`, `fe80::/10`).
  - Loopback (`127.0.0.0/8`, `::1`).
  - Multicast / reserved.
  - Cloud metadata endpoint (`169.254.169.254` already covered).
- Custom `http.Client` with timeouts (`link_fetch_timeout_seconds`), max-redirect count = 5, redirect target re-validated through the same IP check (TOCTOU defence: pin DNS resolution per request).
- `MaxBytesReader` capped at `link_fetch_max_bytes`.

A config flag may permit private IPs for users self-hosting on a LAN, but this is **off by default** (`link_fetch_allow_private_ips: false`).

### 19.8 SQL injection

All queries go through Bun; raw SQL is parameterised. Migrations that use `NewRaw` use static strings only.

### 19.9 XSS on public pages

`/p/:slug` returns JSON, not HTML. The official web frontend renders JSON and is responsible for escaping. We document this contract; we do not strip HTML in note bodies (markdown is rendered client-side by a sanitising renderer).

### 19.10 Secret handling

`.raptor.*.yaml` is in `.gitignore` (already done in `schoolbell`). Only `.raptor.example.yaml` is committed. Documented in `CLAUDE.md` and `README.md`.

---

## 20. Deferred / Future Work

Tracked as the v2 backlog. Order is roughly priority:

1. **API keys** with scopes (read, write, manage_files) for CLI/extensions/agents.
2. **Bookmark imports** — Netscape-bookmark HTML, Pocket CSV. Single-shot upload endpoint that enqueues per-row import jobs.
3. **Browser extension** (Chromium + Firefox) using API keys.
4. **OCR** for images and scanned PDFs (Tesseract via `gosseract` or external `tesseract` binary).
5. **Full-page archival** via `monolith` (subprocess) — stores a single-file HTML snapshot per bookmark.
6. **Semantic / vector search** with `pgvector` and embeddings produced by the LLM provider.
7. **RSS auto-ingest** — per-user feed list, polled on a schedule, new entries become bookmarks.
8. **WebSocket/SSE** for live item state updates (replace polling).
9. **Account export** (JSON dump + all files in a tarball).
10. **Email verification** on registration (toggleable).
11. **SSO** (OIDC).
12. **Shared / collaborative lists** between authenticated users.
13. **Mobile / desktop clients.**
14. **Versioning** of note / markdown bodies.
15. **i18n** (per-user language for FTS dictionary + email templates).

---

## 21. Appendix

### 21.1 Naming conventions cheat sheet

| Item | Convention | Example |
|---|---|---|
| Table | `snake_case`, plural | `bookmark_details` |
| Column | `snake_case`, singular | `url_hash`, `is_public` |
| Go struct | `PascalCase`, singular | `BookmarkDetail` |
| Go field | `PascalCase` | `URLHash` |
| JSON property | `camelCase` | `urlHash` |
| Migration file | `YYYYMMDDHHMMSS_<verb>_<subject>.go` | `20260516120004_create_bookmark_details.go` |
| Controller file | `<resource>_controller.go` | `bookmarks_controller.go` |
| Service file | `<resource>_service.go` | `bookmarks_service.go` |
| Route | `/api/v1/<resource>` | `/api/v1/bookmarks` |
| Controller method | `Index / Show / Create / Update / Destroy / <Custom>` | `Bookmarks.Create` |

### 21.2 Example migration skeleton

```go
// db/migrations/20260516120003_create_items.go
package migrations

import (
    "context"
    "database/sql"

    "github.com/pressly/goose/v3"
    "github.com/uptrace/bun"
    "github.com/uptrace/bun/dialect/pgdialect"

    "github.com/h00s/kluttr/backend/app/models"
)

func init() {
    goose.AddMigrationNoTxContext(upCreateItems, downCreateItems)
}

func upCreateItems(ctx context.Context, sqldb *sql.DB) error {
    db := bun.NewDB(sqldb, pgdialect.New())
    return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
        if _, err := tx.NewCreateTable().Model((*models.Item)(nil)).Exec(ctx); err != nil {
            return err
        }
        // CHECK constraints + composite indexes that Bun tags don't express
        rawStmts := []string{
            `ALTER TABLE items ADD CONSTRAINT items_type_check
               CHECK (type IN ('bookmark','note','markdown','image','pdf'))`,
            `ALTER TABLE items ADD CONSTRAINT items_state_check
               CHECK (state IN ('pending','ready','error'))`,
            `CREATE INDEX items_user_deleted_created_idx
               ON items (user_id, deleted_at, created_at DESC)`,
            `CREATE INDEX items_user_type_idx ON items (user_id, type) WHERE deleted_at IS NULL`,
            `CREATE INDEX items_user_pinned_idx ON items (user_id, created_at DESC) WHERE pinned = true AND deleted_at IS NULL`,
            `CREATE UNIQUE INDEX items_public_slug_idx ON items (public_slug) WHERE is_public = true`,
        }
        for _, q := range rawStmts {
            if _, err := tx.NewRaw(q).Exec(ctx); err != nil {
                return err
            }
        }
        return nil
    })
}

func downCreateItems(ctx context.Context, sqldb *sql.DB) error {
    db := bun.NewDB(sqldb, pgdialect.New())
    _, err := db.NewDropTable().Model((*models.Item)(nil)).IfExists().Cascade().Exec(ctx)
    return err
}
```

### 21.3 Example controller skeleton

```go
// app/controllers/bookmarks_controller.go
package controllers

import (
    "github.com/go-raptor/raptor/v4"
    "github.com/go-raptor/raptor/v4/errs"

    "github.com/h00s/kluttr/backend/app/models"
    "github.com/h00s/kluttr/backend/app/services"
)

type BookmarksController struct {
    raptor.Controller

    Auth      *services.AuthService
    Bookmarks *services.BookmarksService
}

func (c *BookmarksController) Create(ctx *raptor.Context) error {
    user, err := c.Auth.CurrentUser(ctx)
    if err != nil {
        return err
    }

    var req models.BookmarkRequest
    if err := ctx.Bind(&req); err != nil {
        return errs.NewErrorBadRequest("Invalid request payload")
    }
    if err := c.Bookmarks.Validate(req); err != nil {
        return err
    }

    item, err := c.Bookmarks.Create(user, req, ctx.RealIP())
    if err != nil {
        return err
    }
    return ctx.Status(201).Data(item)
}
```

### 21.4 Example service skeleton

```go
// app/services/bookmarks_service.go
package services

import (
    "github.com/go-raptor/raptor/v4"
    "github.com/go-raptor/raptor/v4/errs"

    "github.com/h00s/kluttr/backend/app/models"
    "github.com/h00s/kluttr/backend/app/utils"
)

type BookmarksService struct {
    raptor.Service

    DB     *DatabaseService
    Jobs   *JobsService
    Items  *ItemsService
    Search *SearchService
}

func (s *BookmarksService) Create(user *models.User, req models.BookmarkRequest, ip string) (models.Item, error) {
    norm, hash, err := utils.NormaliseAndHashURL(req.URL)
    if err != nil {
        return models.Item{}, errs.NewErrorBadRequest("Invalid URL")
    }

    var item models.Item
    err = s.DB.Conn().RunInTx(s.DB.Ctx, nil, func(ctx context.Context, tx bun.Tx) error {
        if !req.Force {
            existingID, ok, _ := s.findExistingBookmark(ctx, tx, user.ID, hash)
            if ok {
                return errs.NewErrorConflict("Duplicate URL", "existingItemId", existingID)
            }
        }

        item = models.Item{
            UserID: user.ID,
            Type:   "bookmark",
            Title:  utils.DefaultTitle(req.Title, norm),
            State:  "pending",
        }
        if _, err := tx.NewInsert().Model(&item).Returning("*").Exec(ctx); err != nil {
            return s.DB.HandleError(err)
        }

        bd := models.BookmarkDetail{
            ItemID:  item.ID,
            UserID:  user.ID,
            URL:     norm,
            URLHash: hash,
            Domain:  utils.Hostname(norm),
        }
        if _, err := tx.NewInsert().Model(&bd).Exec(ctx); err != nil {
            return s.DB.HandleError(err)
        }

        if err := s.Items.AttachTagsTx(ctx, tx, user, item.ID, req.Tags, "user"); err != nil {
            return err
        }
        if req.ListID != nil {
            if err := s.Items.AddToListTx(ctx, tx, user, *req.ListID, item.ID); err != nil {
                return err
            }
        }
        if err := s.Search.RefreshTx(ctx, tx, item.ID); err != nil {
            return err
        }
        if err := s.Jobs.EnqueueTx(ctx, tx, jobs.FetchLinkMetadata{ItemID: item.ID}); err != nil {
            return err
        }
        if s.Jobs.LLMEnabled() {
            if err := s.Jobs.EnqueueTx(ctx, tx, jobs.LLMTagSummarize{ItemID: item.ID}); err != nil {
                return err
            }
        }
        return nil
    })
    if err != nil {
        return models.Item{}, err
    }
    return item, nil
}
```

### 21.5 Open questions for the implementer

These are intentionally left open because they are tactical, not architectural:

1. Choice of S3 client library — AWS SDK v2 vs minio-go vs a thin custom wrapper. Decide at impl time based on dep weight.
2. PDF text extraction library — defer; v1 ships without it. When added, evaluate `pdfcpu` vs shelling out to `pdftotext`.
3. Per-user search-language config — out of v1, but the `to_tsvector` call is a single line of code, so designing the column generation to take a config parameter is cheap.
4. Background-job dashboard — none in v1; a basic `/admin/jobs` page is a small v2 addition for the instance owner.

---

*End of specification.*
