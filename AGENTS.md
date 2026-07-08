# zooid

Zooid is a multi-tenant [Nostr](https://nostr.com) relay written in Go, built on
[Khatru](https://gitworkshop.dev/fiatjaf.com/nostrlib/tree/master/khatru), pairing with the
[Flotilla](https://flotilla.social) client as a full NIP-29 community relay. Every "virtual relay"
is one TOML config file; all tenants share a single SQLite database file, isolated by table-name
prefix. `README.md` is the user-facing reference for every config field and env var — read it
first for *what* is configurable. This file and `.agents/skills/zooid/` are the code-level map of
*how* it's actually implemented.

**Before making a non-trivial change, read
[`.agents/skills/zooid/SKILL.md`](.agents/skills/zooid/SKILL.md).** It documents the
multi-tenancy/dispatch model, the four distinct (and easily-conflated) authorization systems, the
event-store internals, and a set of real bugs found during an architecture audit and since fixed
(FTS5 search, an unbounded-query cap, a cross-user invite-lookup bug, transactional writes, a
NIP-98 replay window, push-callback SSRF, a Blossom DELETE nil-pointer panic, a Blossom mirror
SSRF hole, a `listbannedevents` dispatch panic) — knowing that history before you touch nearby
code will save you from either reintroducing them or mistaking the fixes for incidental code
shape. Three of those (both Blossom bugs and `listbannedevents`) were fixed directly in the
vendored `nostrlib` dependency itself, in a sibling checkout at `../nostrlib` — see the skill's
house notes for when to prefer that over a local workaround.

## Tech stack

- Go 1.25, single module `zooid`, single package `zooid/` (no internal sub-packages).
- [`fiatjaf.com/nostr`](https://gitworkshop.dev/fiatjaf.com/nostrlib) (Khatru relay framework,
  nostr types, NIP-29/Blossom helpers) — pinned via a `replace` in `go.mod` to
  `gitea.coracle.social/Coracle/nostrlib`, a Coracle-maintained fork.
- `mattn/go-sqlite3` (cgo) — **all builds need `CGO_ENABLED=1`**.
- `BurntSushi/toml` for config files, `Masterminds/squirrel` for SQL building,
  `fsnotify` for config hot-reload, `spf13/afero` for the local Blossom filesystem adapter,
  `aws-sdk-go-v2` for the S3 Blossom adapter, `livekit/protocol` for LiveKit audio/video.

## Repository layout

```
zooid/            the entire backend — one Go package (see below)
cmd/relay/        the relay server binary (main.go)
cmd/import/       bin/import — reads JSONL from stdin into a virtual relay's event store
cmd/export/       bin/export — writes JSONL to stdout from a virtual relay's event store
config/           default TOML config(s); CONFIG env var points here at runtime
static/, templates/  NIP-11-adjacent static assets served by the relay
media/            local Blossom blob storage (MEDIA env var)
data/             SQLite database + KV store (DATA env var)
bin/              build output (just build-relay/build-import/build-export)
.agents/skills/zooid/   the deep architecture reference — start here for real work
```

### `zooid/` package map

| File | Covers |
|---|---|
| `main_test.go`, `lib.go`, `lib_test.go` | Boot (`Start()`), config-dir scanning, `Dispatch`/`DispatchBySchema`, fsnotify hot reload |
| `config.go`, `config_test.go` | The `Config` struct (TOML+JSON), validation, roles/permissions model |
| `instance.go`, `instance_test.go` | `MakeInstance`/`Cleanup`, all Khatru policy hooks (connect/request/event/broadcast) |
| `events.go`, `events_test.go` | `EventStore` — Khatru's `eventstore.Store` over raw SQL |
| `schema.go`, `schema_test.go` | Per-tenant table-name prefixing (`{schema}__{table}`) |
| `database.go` | The single process-wide `*sql.DB` (shared across all tenants) |
| `kv.go` | A separate, simple namespaced key/value table (little-used) |
| `groups.go`, `groups_test.go` | NIP-29 group membership, roles, pins, metadata |
| `management.go`, `management_test.go` | NIP-86 remote relay management, relay-wide membership, bans, invite claims |
| `api.go`, `api_test.go` | JSON REST API for remotely managing relay *configs* (NIP-98 auth, separate from the Nostr protocol) |
| `blossom.go` | Blossom media storage (local disk or S3) |
| `push.go` | Webhook-style push notifications (kind 30390 subscriptions) |
| `livekit.go` | LiveKit audio/video call integration |
| `util.go`, `env.go` | Shared helpers (NIP-98 validation, generics, event-kind predicates), env var access |

## Build, test, verify

Defined in `justfile`:

```sh
just run            # go run -tags sqlite_fts5 cmd/relay/main.go
just build-relay     just build-import     just build-export     just build
just test           # go test -tags sqlite_fts5 -v ./...
just fmt             # gofmt -w -s .
```

All recipes set `CGO_ENABLED=1` (required by `mattn/go-sqlite3`) **and** pass `-tags sqlite_fts5`
(required to actually compile in SQLite's fts5 extension, which `mattn/go-sqlite3` otherwise
omits) — don't drop either if you invoke `go build`/`go test`/`go run` directly, or full-text
search silently disables itself instead of failing loudly. Package tests share one temp `DATA`
dir per test binary run and rely on running **sequentially** (`Env()` is memoized once via
`sync.Once`; see the skill's "Building and verifying a change" section before adding
`t.Parallel()` anywhere in this package).

## The shape of the codebase, briefly

One `*sql.DB` for the whole process; tenants are isolated purely by SQL table-name prefix, never
by separate database files or connections. A tenant is fully described by its `Config`, built via
`MakeInstance` into an `*Instance` that owns a Khatru `*Relay` plus an `EventStore`. `lib.go`'s
`Start()` and its fsnotify watcher are the only things that create/replace/tear down `Instance`s at
runtime — there's no other reload mechanism. Four different, non-interchangeable systems govern
"who can do what" (config-driven roles, relay-wide membership, cosmetic role labels, per-group
NIP-29 membership) — see the skill before changing anything permission-related.

For the full picture — exact table/index shapes, the NIP-86 method dispatch table, the precise
order of every policy-hook check, and the full history of what was found broken in an
architecture audit and fixed since (with regression tests) — see
[`.agents/skills/zooid/SKILL.md`](.agents/skills/zooid/SKILL.md) and its `references/` directory.

## Skills

Skills live in `.agents/skills/` and should be created/updated whenever you learn something about
this codebase that isn't obvious from reading the code fresh — before writing a new one, check
whether an existing skill already covers the area.
