# Kluttr — Project Guide

> Read this first. The full design lives in [`docs/backend-specification.md`](docs/backend-specification.md). This file is for fast orientation.

## What is Kluttr?

Kluttr is a **self-hostable, multi-user, "bookmark everything" web app**. Users save links, notes, markdown documents, images, and PDFs; organise them into lists; tag them (manually or automatically by an LLM); full-text-search everything; and optionally publish items or lists as read-only public pages.

It is inspired by Hoarder but **deliberately smaller**: core features only, fast, lightweight, single Go binary + Postgres.

**Non-goals (v1):** API keys, browser extensions, mobile apps, imports, full-page archival, OCR, RSS, semantic search, WebSocket/SSE, SSO, sharing between authenticated users. See spec §20 for the v2 backlog.

## Repo layout

```
kluttr/
├── backend/          # Go + Raptor v4 — the API server (this is where almost all code lives)
├── docs/             # Specifications and design docs
│   └── backend-specification.md      # source of truth for the backend
├── CLAUDE.md         # this file
└── (frontend/)       # planned, not yet started
```

## Backend stack

| Concern | Choice |
|---|---|
| Language / framework | Go + [Raptor v4](https://github.com/go-raptor/raptor) |
| DB | PostgreSQL via [Bun ORM](https://github.com/uptrace/bun) (driver: `pgx/v5`) |
| Connector | `github.com/go-raptor/connectors/bun/postgres` |
| Migrations | [Goose](https://github.com/pressly/goose), Go-based, registered via `init()` |
| Validation | [Zog](https://github.com/Oudwins/zog) |
| In-memory cache | [Ristretto v2](https://github.com/dgraph-io/ristretto) |
| Logging | `log/slog` + [tint](https://github.com/lmittmann/tint) handler |
| Password hashing | Argon2id (`golang.org/x/crypto/argon2`) |
| Auth | Opaque Bearer tokens, sha256-hashed at rest — **no JWT** |
| Async work | In-process goroutine workers + Postgres `jobs` table with `SKIP LOCKED` |
| LLM | Provider-pluggable Go interface; v1 adapters: OpenAI, Ollama |
| File storage | `Storage` interface; v1 adapters: local FS, S3-compatible |

The patterns mirror the existing Raptor app at `/home/h00s/dev/go/schoolbell/backend` — when in doubt, look there.

## Where things live

```
backend/
├── main.go                            # entry point — unchanged from Raptor scaffold
├── .raptor.dev.yaml                   # local dev config (gitignored; .example.yaml is committed)
├── app/
│   ├── controllers/<resource>_controller.go    # thin: bind, validate, call service
│   ├── middlewares/auth_middleware.go          # Bearer → Session injection
│   ├── models/<name>.go                        # Bun struct + Request/Response + Zog schema
│   ├── services/<resource>_service.go          # business logic; depends on DatabaseService
│   ├── storage/{storage,local,s3}.go           # Storage interface + adapters
│   ├── llm/{llm,openai,ollama}.go              # LLMProvider interface + adapters
│   ├── jobs/<job_type>.go                      # job handlers (pure functions)
│   └── utils/                                  # urlnorm, slug, httpfetch (SSRF-safe)
├── config/
│   ├── routes.yaml                             # all routes (embedded into routes.go)
│   ├── routes.go                               # parses routes.yaml at startup
│   └── components/
│       ├── components.go                       # New() wires Connector + Services + Middlewares + Controllers
│       ├── controllers.go                      # one slice entry per controller
│       ├── services.go                         # one slice entry per service
│       └── middlewares.go                      # auth applied via core.UseExcept(...)
└── db/
    ├── migrations.go                           # MigrationsFS() (returns nil; init()s register migrations)
    └── migrations/YYYYMMDDHHMMSS_<verb>_<subject>.go
```

## Naming conventions

| Item | Convention | Example |
|---|---|---|
| Table | snake_case, plural | `bookmark_details` |
| Column | snake_case, singular | `url_hash`, `is_public` |
| Go struct | PascalCase, singular | `BookmarkDetail` |
| JSON property | camelCase | `urlHash` |
| Bun tag | `bun:"col_name,notnull,..."` on the model field | see `app/models/*.go` |
| Migration file | `YYYYMMDDHHMMSS_create_<resource>.go` | `20260516120003_create_items.go` |
| Controller method | `Index / Show / Create / Update / Destroy / <Custom>` | `Bookmarks.Create` |
| Route group | `/api/v1/<resource>` | `/api/v1/bookmarks` |

## Adding a feature (recipe)

To add a new resource (say `Foo`):

1. **Model** — create `app/models/foo.go` with:
   - `type Foo struct { bun.BaseModel ...; ID int64 ...; UserID int64 ...; ...; CreatedAt; UpdatedAt }`
   - `type FooRequest`, `type FooResponse` (+ `NewFooResponse(...)` helper) in the same file
   - A Zog schema function `FooSchema() *zog.StructSchema` for validation
2. **Migration** — create `db/migrations/<timestamp>_create_foos.go` following the `init() / upCreateFoos / downCreateFoos` pattern. Use `tx.NewCreateTable().Model((*models.Foo)(nil))` for the table, raw SQL inside the same tx for CHECK constraints and indexes. Add the foreign key to the **final** `add_foreign_keys` migration rather than this one.
3. **Service** — create `app/services/foos_service.go` with a struct embedding `raptor.Service`, dependencies (`DB *DatabaseService`, `Jobs *JobsService`, …), and methods (`Create`, `Get`, `List`, `Update`, `Destroy`, etc.). All DB writes that produce side-effects (jobs, search refresh) wrap in `s.DB.Conn().RunInTx(...)`.
4. **Controller** — create `app/controllers/foos_controller.go` with `FoosController` embedding `raptor.Controller`. Each action is `func (c *FoosController) <Action>(ctx *raptor.Context) error`: bind → validate → call service → return.
5. **Register** — add `&controllers.FoosController{}` to `config/components/controllers.go` and `&services.FoosService{}` to `config/components/services.go`. Add the Zog schema to `ValidationService` in `app/services/validation_service.go`.
6. **Routes** — add the routes to `config/routes.yaml`. Example:
   ```yaml
   /foos:
     GET: Foos.Index
     POST: Foos.Create
     /{id}:
       GET: Foos.Show
       PATCH: Foos.Update
       DELETE: Foos.Destroy
   ```
   Authentication is applied by default; to expose anonymously, add the controller method to the exclusion list in `config/components/middlewares.go` (`core.UseExcept(...)`).

## Auth model summary

- Login (`POST /api/v1/auth/login`) returns an opaque random token. The server stores only `sha256(token)` in `sessions.token`.
- The client sends `Authorization: Bearer <token>` on every subsequent request.
- `AuthMiddleware` parses the header, looks up `sha256(token)` in the Ristretto cache (DB fallback), and injects `*Session` into the request context.
- `AuthService.CurrentUser(ctx) / CurrentSession(ctx)` retrieve the principal in service code.
- No JWT, no refresh tokens. Logout deletes the session row.

See spec §6 and `schoolbell`'s `sessions_service.go`/`auth_middleware.go` for the reference implementation.

## Background jobs summary

- The `jobs` table is the queue. Workers are goroutines started by `JobsService.Setup()`.
- To enqueue from a service, use `s.Jobs.EnqueueTx(ctx, tx, payload)` inside the same Bun tx as the originating write — transactional enqueue.
- To add a new job type:
  1. Define `type FooBar struct { ... }` in `app/jobs/foo_bar.go` with a `JobType() string` method returning the snake_case name.
  2. Add the handler function `func HandleFooBar(ctx context.Context, deps Deps, payload FooBar) error`.
  3. Register the handler in `JobsService.Setup()` (the registry map keyed by `JobType()`).
- Failures retry with exponential backoff up to `job_max_attempts`, then transition to state `dead` with `last_error` recorded.

See spec §13.

## LLM and Storage abstractions

- **LLM**: `app/llm/llm.go` defines `Provider` interface with a single `Suggest(ctx, in) (out, error)` method returning `{tags, summary, category}`. Adapters: `openai.go`, `ollama.go`. Selected by `app.llm_provider` config. Called only from the `llm_tag_summarize` job handler. See spec §11.
- **Storage**: `app/storage/storage.go` defines `Storage` interface with `Put / Get / Delete / SignedURL`. Adapters: `local.go` (filesystem), `s3.go` (S3-compatible). Selected by `app.storage_backend` config. The `FilesService` and the `files_controller` use it through `StorageService`. See spec §12.

When changing either subsystem, **change the interface in one place** and update both adapters in parallel.

## Configuration

- Local dev: `.raptor.dev.yaml` (gitignored). Bootstrap from `.raptor.example.yaml`.
- Production: `.raptor.production.yaml` or env-driven (Raptor supports env override on top of YAML).
- The full schema and every key Kluttr adds under `app:` is documented in spec §5.
- **Never** commit `.raptor.dev.yaml` or `.raptor.production.yaml`. Only `.raptor.example.yaml` is in git.

## Local development

```bash
cd backend
# one-time: install raptor CLI
go install github.com/go-raptor/raptor/v4/cmd/raptor@latest

# start dev server (auto-reload on file change)
raptor dev
```

- The server listens on `http://localhost:3000` (per `.raptor.dev.yaml`).
- Frontend dev origin is `http://localhost:5173` (CORS allow-list).
- Postgres must be reachable per `.raptor.dev.yaml`'s `database:` block. Migrations run automatically at startup.

### Running migrations

Migrations run automatically when the app boots (the `bun/postgres` connector invokes Goose against `db/migrations.MigrationsFS()`). To run them manually:

```bash
go run github.com/pressly/goose/v3/cmd/goose@latest -dir=db/migrations status
```

(Note: schoolbell's pattern registers migrations via `init()` rather than via the `dir` flag; the CLI is mostly useful for `status` and `down`.)

### Tests

```bash
go test ./...
```

Controller tests follow the Raptor pattern (`*_test.go` next to the file). Services that touch the DB use a real Postgres via `.raptor.test.yaml`.

## Reference: spec section index

| Topic | Spec section |
|---|---|
| High-level overview & goals | §1 |
| Architecture diagram | §2 |
| Tech stack table | §3 |
| Project layout | §4 |
| Configuration keys | §5 |
| Auth & sessions | §6 |
| Data model (all tables) | §7 |
| Content pipelines (bookmarks/notes/files) | §8 |
| Lists / tags / highlights | §9 |
| Search (FTS + trigram) | §10 |
| LLM integration | §11 |
| File storage | §12 |
| Background jobs | §13 |
| API conventions (errors, pagination, etc.) | §14 |
| Endpoint catalog | §15 |
| Public sharing (`/p/:slug`) | §16 |
| Soft-delete & trash | §17 |
| Observability | §18 |
| Security checklist | §19 |
| Deferred features / v2 backlog | §20 |
| Naming + example skeletons | §21 |

## When in doubt

- Existing Raptor app to mirror: `/home/h00s/dev/go/schoolbell/backend`
- Spec lives at `docs/backend-specification.md` — it is opinionated; follow it. If reality conflicts with the spec, update the spec in the same PR as the code.
