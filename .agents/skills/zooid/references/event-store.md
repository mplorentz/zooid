# Event store, schema, and KV (deep detail)

This is the lookup-depth companion to the SKILL.md "storage" section: the exact table/index
shapes, the query-building algorithm, replaceable/addressable semantics, and the history of a set
of real correctness bugs found here in an architecture audit and since fixed (kept documented
because the fixes explain non-obvious shapes in the current code). All of it lives in
`zooid/events.go`, `zooid/schema.go`, `zooid/database.go`, `zooid/kv.go`.

## 1. `EventStore` struct and interface

```go
type EventStore struct {
	Relay        *khatru.Relay
	Config       *Config
	Schema       *Schema
	FTSAvailable bool
}
var _ eventstore.Store = (*EventStore)(nil)
```

`eventstore.Store` (vendored, `nostrlib/eventstore/store.go`) requires: `Init() error`, `Close()`,
`QueryEvents(nostr.Filter, maxLimit int) iter.Seq[nostr.Event]`, `DeleteEvent(nostr.ID) error`,
`SaveEvent(nostr.Event) error`, `ReplaceEvent(nostr.Event) (deleted []nostr.Event, err error)`,
`CountEvents(nostr.Filter) (uint32, error)`. Implemented at `events.go:29` (`Init`), `:109`
(`Close`), `:113` (`QueryEvents`), `:299` (`DeleteEvent`), `:408` (`SaveEvent`), `:414`
(`ReplaceEvent`), `:472` (`CountEvents`). `Relay` is optional — `cmd/import`/`cmd/export`
construct `EventStore{Config, Schema}` with `Relay` left `nil`; the only dereference of it
(`SignAndStoreEvent`'s broadcast) guards for nil.

## 2. Storage schema (`events.go:31-58`, rendered via `Schema.Render`)

```sql
CREATE TABLE IF NOT EXISTS {{.Name}}__events (
	id TEXT PRIMARY KEY, created_at INTEGER NOT NULL, kind INTEGER NOT NULL,
	pubkey TEXT NOT NULL, content TEXT NOT NULL, tags TEXT NOT NULL, sig TEXT NOT NULL
);
CREATE INDEX ..._idx_events_created_at (created_at)
CREATE INDEX ..._idx_events_kind (kind)
CREATE INDEX ..._idx_events_pubkey (pubkey)
CREATE INDEX ..._idx_events_kind_pubkey (kind, pubkey)
CREATE INDEX ..._idx_events_kind_pubkey_created_at (kind, pubkey, created_at DESC)

CREATE TABLE IF NOT EXISTS {{.Name}}__event_tags (
	event_id TEXT NOT NULL, key TEXT NOT NULL, value TEXT NOT NULL,
	FOREIGN KEY (event_id) REFERENCES {{.Name}}__events(id) ON DELETE CASCADE
);
CREATE INDEX ..._idx_event_tags_event_id (event_id)
CREATE INDEX ..._idx_event_tags_key (key)
CREATE INDEX ..._idx_event_tags_key_value (key, value)
```

`tags TEXT` holds the whole tag array as a JSON blob (round-tripped whole for reconstructing a
`nostr.Event`). `event_tags` indexes **only single-letter tag keys** — `saveEvent` filters with
`len(tag[0]) == 1` (`events.go:349`), matching the NIP-01/NIP-12 convention that only
single-letter tags are queryable via `#<letter>` filters; multi-letter tags (`title`, `alt`,
`claim`, …) live only inside the JSON blob and are never filterable at the SQL layer. The
`kind_pubkey_created_at` composite index exists specifically to serve `ReplaceEvent`'s lookup
(kind+author, ordered by recency) and the common `{kinds, authors}` filter without a full scan.
`_foreign_keys=true` in the DSN (`database.go:16`) is what makes `ON DELETE CASCADE` actually
fire per-connection (SQLite disables FK enforcement by default) — so `DeleteEvent`'s plain
`DELETE FROM events WHERE id=?` transitively removes the row's tags with no explicit code doing
so.

### FTS5 — fixed, but requires a build tag

Full-text search used to be silently non-functional for two independent reasons, both now fixed:

1. **The fts5 schema string wasn't rendered.** `ftsSchema` used to be a plain Go string literal
   containing literal `{{.Name}}` template syntax instead of being passed through
   `Schema.Render` like `basicSchema` — `{`, `}`, and unescaped `.Name` aren't valid unquoted
   SQLite identifier characters, so `GetDb().Exec(ftsSchema)` failed a SQL syntax error on every
   call. It's now built via `events.Schema.Render(...)` (`events.go:31,60-84`), same as the base
   schema.
2. **`mattn/go-sqlite3` doesn't compile in the fts5 SQLite extension unless built with the
   `sqlite_fts5` (or `fts5`) build tag** (`.../mattn/go-sqlite3@.../sqlite3_opt_fts5.go` — gated
   behind `//go:build sqlite_fts5 || fts5`). Without it, `CREATE VIRTUAL TABLE ... USING fts5(...)`
   fails with "no such module: fts5" regardless of whether the template renders correctly.
   `justfile`'s `run`/`build-relay`/`build-import`/`build-export`/`test` recipes all pass
   `-tags sqlite_fts5` for exactly this reason — building or testing this package without that tag
   (e.g. a bare `go build ./...`) silently disables search (`Init()` sets `FTSAvailable = false`
   and moves on) rather than failing loudly.

`Init()` now also runs `INSERT INTO {{.Name}}__events_fts({{.Name}}__events_fts) VALUES('rebuild')`
after creating the fts5 table (`events.go:86-92`) — idempotent and safe to run on every startup —
so events written before either fix (or before fts5 was available at all) become searchable too,
not just newly-written ones. If the rebuild fails, `FTSAvailable` is left `false` rather than
advertising a possibly-incomplete index.

The search query itself (`buildSelectQuery`, `events.go:203-211`) also had two bugs beyond the
schema never existing: it joined on `Where(squirrel.Eq{"events_fts": filter.Search})` — using `=`
instead of FTS5's `MATCH` operator, and referencing the bare literal column name `"events_fts"`
rather than the tenant-prefixed table. Fixed to `Where(fmt.Sprintf("%s MATCH ?", ftsTable),
phrase)`, with `phrase` the search term wrapped in a quoted FTS5 phrase (embedded `"` doubled) so
that arbitrary user input containing FTS5 query-syntax characters (hyphens, colons, boolean
keywords, unbalanced quotes) is treated as a literal phrase rather than breaking the `MATCH`
expression. The non-FTS fallback (`LIKE "%term%"`, still used whenever `FTSAvailable` is false)
is unindexed by nature — that's inherent to `LIKE` with a leading wildcard, not a bug.

## 3. Query building — `buildSelectQuery` (`events.go:188-262`)

Built with squirrel's default `?`-placeholder builders (correct for `mattn/go-sqlite3`). Selects
seven columns, each qualified with the events table name (`{schema}__events.id`, `.created_at`,
etc. — qualification matters once the fts5 join is present, since both tables declare a `content`
column and an unqualified reference would be ambiguous), `ORDER BY {events}.created_at DESC`
**unconditionally** — there is no ascending-order option; every filter is served newest-first.

- `filter.Search` → FTS5 `MATCH` join (§2) when `FTSAvailable`, else `LIKE "%term%"`.
- `filter.IDs`/`.Authors`/`.Kinds` → `squirrel.Eq{col: [...]}` → SQL `IN (...)`, each hits its
  single-column index or the PK. (These bare column names stay unambiguous even with the fts5
  join present, since the fts5 table declares no `id`/`kind`/`pubkey`/`created_at` column.)
- `filter.Since`/`.Until` → `created_at >= ?` / `<= ?`, both inclusive.
- `filter.Tags` (`map[string][]string]`) → skipped unless `len(tagKey) == 1` (`events.go:237-239`)
  — same single-letter convention as storage. Each surviving key becomes a correlated subquery
  `SELECT event_id FROM event_tags WHERE key=? AND value IN (?,...)`, spliced in as `id IN
  (<subquery>)`. Multiple distinct tag keys AND together; multiple values for the same key OR via
  the subquery's `IN` — standard NIP-01 semantics. **`TestEventStore_QueryEvents_ByTags`
  documents that an unsupported (multi-char) tag key doesn't just get ignored for that key — it
  drops the entire tag filter, so a filter like `{"title":["x"]}` returns *all* stored events
  rather than zero.** Worth remembering when reasoning about "will this filter over-return data" —
  this one is existing, intentional-by-inaction behavior, not something the audit's fixes touched.
- `filter.Limit` → only applied to SQL if `filter.Limit > 0` (`events.go:255-257`); `Limit == 0`
  gets no SQL `LIMIT` from `buildSelectQuery` itself — capping a limit-less filter is
  `QueryEvents`'s job via `maxLimit`, described next.

### Fixed — `maxLimit` now actually caps an unlimited filter

```go
// events.go:127-133 (queryEvents, the shared implementation behind QueryEvents)
if maxLimit > 0 && (filter.Limit == 0 || filter.Limit > maxLimit) {
	filter.Limit = maxLimit
}
```
Previously this read `if maxLimit > 0 && maxLimit < filter.Limit`, which only lowered
`filter.Limit` when the caller's filter *already* specified a limit greater than `maxLimit`. When
`filter.Limit == 0` (the normal "no limit given" case per NIP-01), that condition was false,
`filter.Limit` stayed `0`, and `buildSelectQuery` never added a `LIMIT` — the query ran completely
unbounded. Concretely, `instance.QueryStored` calls `Events.QueryEvents(filter, 1000)` for
ordinary requests — a client sending `{"kinds":[1]}` with no `limit` now genuinely gets at most
1000 rows, matching what the code always intended. `ReplaceEvent`'s internal query and the
`GetOrCreate*` helpers pass `1` expecting "at most one row"; before the fix this was only true
because the *Go* call sites manually took just the first iterated row, not because the SQL was
actually limited — it's now enforced at the SQL layer too, which matters for `ReplaceEvent`
specifically: it deliberately queries with `maxLimit=0` (unbounded) rather than `1`, because unlike
those single-row lookups it needs to see and clean up *every* existing event for a given
kind/author[/d] combination, not just the first (see §4).

### Fixed — `CountEvents` no longer double-applies `Limit`

`CountEvents` (`events.go:472-491`) resets `filter.Limit = 0` on a local copy before calling
`buildSelectQuery`, so any `Limit` the caller set (a delivery/pagination hint meant for
`QueryEvents`) no longer also caps the count. Previously it wrapped `buildSelectQuery(filter)`
(including whatever `LIMIT` that produced) inside `SELECT COUNT(*) FROM (subquery)`, so a
`CountEvents` call with a nonzero `filter.Limit` returned `min(true_count, filter.Limit)` instead
of the true count. No current call site (`push.go`'s subscription-count check) happened to set
`Limit`, so this wasn't exercised in production, but it was a real trap in the public interface
contract — `TestEventStore_CountEvents_IgnoresFilterLimit` locks in the fix.

## 4. Store / replace / delete semantics

### Fixed — transactional, busy-retrying writes with no check-then-insert race

`saveEvent(runner squirrel.BaseRunner, evt nostr.Event) error` (`events.go:311-360`) is the shared
implementation behind `SaveEvent`. It no longer does a separate `SELECT id ... WHERE id=?`
existence check before inserting — it relies on the `id TEXT PRIMARY KEY` constraint itself:
if the `INSERT` fails with a uniqueness violation (`isUniqueConstraintErr`, `events.go:283-291`,
checking `sqlite3.Error{Code: sqlite3.ErrConstraint}`'s extended code for `ErrConstraintUnique`/
`ErrConstraintPrimaryKey`), it returns `eventstore.ErrDupEvent` directly. This removes the
check-then-insert race entirely (there's no window between a check and an insert for a concurrent
save of the same event to slip through) rather than trying to close it. The event row and its
single-letter tag rows are written in the same caller-supplied transaction, and a failed tag
insert now returns an error (`events.go:353-355`) instead of being silently swallowed — previously
`if err != nil { continue }`, with a comment claiming "log error" but nothing actually logged,
meaning an event could go on being fully queryable by id/author/kind while silently invisible to
tag-filtered queries. `TestEventStore_SaveEvent_ConcurrentDuplicate` exercises 8 goroutines racing
to save the same event and asserts exactly one succeeds and the rest get `ErrDupEvent`, not a
generic wrapped SQL error.

`SaveEvent`/`ReplaceEvent` both run through `runInTx(fn func(tx *sql.Tx) error) error`
(`events.go:362-401`), which begins a transaction, runs `fn`, and commits — retrying the **whole
attempt from scratch with a fresh transaction** (not just waiting inside the same one) up to 5
times with a short linear backoff whenever the begin/`fn`/commit fails with
`isBusyErr` (`events.go:403-406`, matching `sqlite3.ErrBusy`, which also covers the
`ErrBusySnapshot` extended code). This was added because wrapping saves/replaces in an explicit
transaction turned out to make `SQLITE_BUSY`/"database is locked" errors from genuinely concurrent
writers (multiple goroutines saving at once) surface more easily than the old
autocommit-per-statement code — and, specifically for the `BUSY_SNAPSHOT` variant that can occur
under WAL mode when a transaction's read snapshot goes stale before it upgrades to a write lock,
simply waiting longer inside the same transaction doesn't help; only starting over with a new one
does. `TestEventStore_SaveEvent_ConcurrentDuplicate` is what surfaced this in the first place (it
was flaky against a naive `tx.Begin()`-once implementation until the retry wrapper was added).

**Why no transaction/duplicate-check bug ever needed a schema migration:** the fix is purely in
how existing statements are sequenced and wrapped; the table/column shapes in §2 are unchanged.

### `ReplaceEvent` (`events.go:414-470`)

Filters on `{Kinds:[evt.Kind], Authors:[evt.PubKey]}`, adding `Tags:{"d":[evt.Tags.GetD()]}` only
if `evt.Kind.IsAddressable()` (30000–39999) — the standard replaceable-vs-parameterized split
(`IsReplaceable()` = kind 0, 3, or 10000–19999). Iterates matching previous events (queried within
the same transaction, `maxLimit=0`/unbounded — see §3): `CreatedAt <= evt.CreatedAt` → delete; a
strictly newer one flips `shouldSave = false`. New event saved first (if `shouldSave`), old ones
deleted after ("just in case our new one doesn't save") — now all inside one transaction via
`runInTx`, so a failure partway through rolls back the whole replace instead of leaving duplicate
rows for the same `(kind, author[, d])` tuple.

**Deliberately does not implement NIP-01's tie-break recommendation.** The spec suggests that on
an exact `created_at` tie, the event with the lowest id (lexically) should be kept. An earlier
attempt to implement exactly that broke four existing tests
(`TestGroupStore_UpdatePins`, `TestManagementStore_AllowPubkey`, `_UnbanPubkey`,
`_AssignAndUnassignRole`) — because zooid uses replaceable/addressable kinds as its own internal,
single-writer state store (banned lists, member lists, role assignments, group pins, invite
claims, ...), two updates landing in the same wall-clock second are routine (sub-second admin
operations, not independent clients racing), and they must resolve to "whichever was processed
last wins," never to an outcome that depends on how the event's content happens to hash. Keep the
`previous.CreatedAt <= evt.CreatedAt` comparison as-is; do not "fix" it toward spec-literal
lowest-id-wins without first checking whether that breaks the same four tests again.

### Deletion (NIP-09) — unchanged, mechanism only

Not implemented in zooid at all — `EventStore.DeleteEvent`/`deleteEvent` (`events.go:293-301`) is
a dumb "delete by id" with no author/authorization check. NIP-09 tag parsing and the "is this the
author (or `AllowDeleting`-approved)" check live entirely upstream in the vendored khatru's
`handleDeleteRequest`, which calls `rl.QueryStored`/`rl.DeleteEvent` (wired to
`instance.DeleteEvent`/`instance.QueryStored`, which forward straight to `EventStore`). Zooid's
storage layer is deletion-*mechanism* only; deletion-*policy* is upstream.

### Redundant-but-harmless double-check — unchanged

Khatru's own dispatch (`handleNormal`) already branches on `evt.Kind.IsRegular()` to call either
`StoreEvent`/`ReplaceEvent` (never both), and routes `IsEphemeral()` kinds to a path that never
touches `Store` at all. Zooid's own `EventStore.StoreEvent` (`events.go:496-506`) redundantly
re-checks `IsReplaceable()||IsAddressable()` before delegating — dead weight for the normal
websocket path, but exactly what makes `StoreEvent` safe to call directly and generically from
`cmd/import` and every `SignAndStoreEvent` call site that doesn't go through khatru's dispatch at
all.

## 5. Zooid-specific convenience constructors

- `GetOrCreateApplicationSpecificData(d string)` (`events.go:525-544`) — queries
  `{Kinds:[KindApplicationSpecificData], Tags:{"d":[d]}}`, else returns an *unsigned* in-memory
  event for the caller to sign/store. Used for the ban lists and other `zooid/`-prefixed
  per-relay config storage (see `groups-and-management.md`).
- `GetOrCreateRelayMembersList()` (`events.go:546-561`) — queries `{Kinds:[RELAY_MEMBERS]}`
  (13534, replaceable range), else returns an unsigned event with a protected `["-"]` tag.

### Fixed — `Instance.GenerateInviteEvent`'s tag-key bug

`Instance.GenerateInviteEvent` (`instance.go:208-234`, called from `QueryStored`) builds its "does
an invite already exist for this pubkey" existence-check filter. It used to read:
```go
Tags: nostr.TagMap{"#p": []string{pubkey.Hex()}}   // WRONG — every other TagMap use in this
                                                    // codebase uses the bare letter
```
`nostr.TagMap` is populated with the bare letter internally everywhere else (`"d"`, `"h"`, `"p"`,
`"t"` — see `events.go`, `groups.go`). Since `buildSelectQuery`'s tag loop drops any key with
`len(tagKey) != 1` (§3), `"#p"` (length 2) was silently dropped from the WHERE clause entirely —
the existence check matched **every** `RELAY_INVITE` event regardless of target pubkey, and
(combined with the `maxLimit` bug in §3, meaning no SQL `LIMIT` either, pre-fix) returned the
most-recently-inserted `RELAY_INVITE` row in the whole tenant. If pubkey B was invited more
recently than pubkey A, calling `GenerateInviteEvent(A)` would return **B's** invite/claim event,
`p`-tagged for B, mislabeled as A's — a genuine cross-user correctness bug, not just a style
inconsistency, given the codebase's own well-established and tested `TagMap` convention
everywhere else. Fixed to the bare `"p"` key.
`TestInstance_GenerateInviteEvent_ReturnsOwnInvite` invites two different pubkeys and asserts the
second lookup for the first pubkey still returns that pubkey's own invite.

## 6. Concurrency & correctness notes (consolidated, current state)

1. Writes go through `runInTx`, which retries the whole transaction (fresh `BEGIN`, not a wait)
   on `SQLITE_BUSY`/`SQLITE_BUSY_SNAPSHOT` up to 5 times with linear backoff.
2. Tag-insert failures now propagate and roll back the whole save instead of being swallowed.
3. `maxLimit` correctly caps `Limit==0` filters.
4. `CountEvents` no longer double-applies `Limit`.
5. `FTSAvailable` reflects real fts5 availability (requires the `sqlite_fts5` build tag) and
   `Init()` rebuilds the index on every startup so historical events stay searchable.
6. Duplicate-detection has no check-then-insert race; concurrent saves of the same event
   deterministically yield exactly one success and `ErrDupEvent` for the rest.
7. `ReplaceEvent` intentionally keeps "last processed wins" on an exact `created_at` tie rather
   than NIP-01's lowest-id-wins recommendation (§4) — this is a deliberate divergence from the
   external protocol spec to match zooid's own internal usage, not a gap.
8. `GenerateInviteEvent`'s tag-key bug is fixed; it now correctly scopes by pubkey.
9. `Schema.Render` still calls `log.Fatal` on template error (`schema.go:16-18`) — a templating
   failure would kill the **entire process** (every tenant), not just the offending relay's
   `Init()`. Low likelihood given the static templates in use (this wasn't part of the fixed bug
   set — it's a latent blast-radius mismatch, not something triggered by any known input), but a
   notable one worth knowing about if `Schema.Render` callers ever change.
10. `mattn/go-sqlite3` sets `PRAGMA busy_timeout = 5000` on every connection unconditionally
    (whether or not `_busy_timeout` is in the DSN) — this is necessary but was not, by itself,
    sufficient to prevent "database is locked" under the concurrent-writer test in point 1; the
    retry-with-fresh-transaction behavior in `runInTx` is what actually closes the gap, per the
    `SQLITE_BUSY_SNAPSHOT` reasoning above.
11. `EventStore.Close()` intentionally never closes the DB — correct for the shared-singleton
    design (`database.go`), but means nothing in this type is safe to assume closed after some
    *other* tenant's shutdown path — there is exactly one `*sql.DB` for the whole process.

## 7. Test-derived behavior

- `createTestEventStore()` builds a fresh `Schema{Name: "test_" + RandomString(8)}` per test —
  confirms tenants are fully isolated by table-name prefix alone on the same physical DB file, and
  that `EventStore` needs only `Config`+`Schema` (no `Relay`) to function, matching `cmd/import`/
  `cmd/export`'s usage.
- `TestEventStore_SaveEvent_ConcurrentDuplicate` (new) exercises 8 concurrent goroutines saving
  the identical event; asserts exactly 1 success and 7 `eventstore.ErrDupEvent`, never a generic
  error — this is what drove the `runInTx` busy-retry design (see §4/§6).
- `TestEventStore_QueryEvents_MaxLimitCapsFilterWithNoLimit` and
  `TestEventStore_CountEvents_IgnoresFilterLimit` (new) lock in the two query-building fixes.
- `TestEventStore_Init_FTSAvailable` and `TestEventStore_QueryEvents_SearchUsesFTS` (new) assert
  `FTSAvailable == true` after `Init()` and that a `MATCH`-based search returns exactly the
  matching event, including one with FTS5 query-syntax characters in its content (quotes, hyphens,
  colons) to confirm the phrase-quoting escape works. These will `t.Skip` if run without the
  `sqlite_fts5` build tag, since `FTSAvailable` will correctly be `false` in that case.
- `TestEventStore_QueryEvents_LimitZero` confirms `filter.LimitZero` short-circuits `QueryEvents`
  to zero rows unconditionally, independent of any other filter field or the `maxLimit` argument
  — the one limit-related control flow that was already correct before any of this.
- `TestEventStore_ReplaceEvent`/`_OlderEvent` cover strictly-newer/strictly-older replacement; no
  test exercises the exact-timestamp tie case directly (the four tests that broke when tie-break
  was changed are indirect coverage of it, via NIP-29/NIP-86 flows rather than `events_test.go`
  itself).
- `TestEventStore_GetOrCreateApplicationSpecificData` shows the intended two-phase pattern: get-
  or-build an *unsigned* event, mutate/inspect it, then `SignAndStoreEvent` — confirming the
  `GetOrCreate*` helpers are meant to be composed with `SignAndStoreEvent`, not used standalone.
- `schema_test.go` only asserts pure string-templating behavior of `Render`/`Prefix` — no DB
  interaction, no sanitization of `Name` beyond what `config.go`'s regex already guarantees.

## Sources

- interface/struct — `zooid/events.go:1-27`
- schema + FTS fix — `zooid/events.go:29-172`
- query building + `maxLimit`/`CountEvents` fixes — `zooid/events.go:113-186,472-491`
- save/replace/delete semantics + `runInTx` — `zooid/events.go:283-470,496-506`
- invite-event tag-key fix — `zooid/instance.go:208-234`
- build tag requirement — `justfile`, `Dockerfile`,
  `.../mattn/go-sqlite3@.../sqlite3_opt_fts5.go`
- KV store (separate, simple key/value table, namespaced via `KV{Name}`) — `zooid/kv.go:1-96`
- shared DB singleton, DSN, WAL/FK pragmas — `zooid/database.go:1-26`
- regression tests — `zooid/events_test.go` (new tests appended at end of file)
