# Config and instance lifecycle (deep detail)

This is the lookup-depth companion to the SKILL.md "config" and "instance lifecycle" sections:
the full `Config` field surface, the exact validation order, the roles/permission model, and the
hot-reload hazards. All of it lives in `zooid/config.go`, `zooid/instance.go`, `zooid/lib.go`,
`zooid/schema.go`, `zooid/database.go`.

## 1. `Config` struct shape (`config.go:14-80`)

One `Config` struct serves both the TOML file format (disk) and the JSON REST API body
(`api.go`) — same field tags, both loaders funnel into the same `Validate()`.

- `Host string` (`config.go:21`) — required (`config.go:125-127`).
- `Schema string` (`config.go:22`) — required (`config.go:129-131`), and constrained to
  `^[a-z_][a-z0-9_]*$` (`config.go:133-135`). **This regex is the only place a schema name is
  validated as a safe SQL identifier** — see §6.
- `Secret string` (`config.go:23`) — parsed via `nostr.SecretKeyFromHex` (`config.go:137-143`).
  No explicit "required" check exists (unlike Host/Schema); an empty string is only rejected as
  an emergent side effect of zero-padding + an all-zero-key check inside the vendored parser —
  see §6.
- `Inactive bool` (`config.go:24`) — read by `lib.go`/callers, **not** by `Validate()` or
  `MakeInstance` — an inactive config still fully builds an `Instance` (tables created, stores
  wired) before the caller discards it. See §6.
- `Info` anonymous struct (`config.go:25-30`): `Name`, `Icon`, `Pubkey`, `Description`. Only
  `Pubkey` is validated (`config.go:145-147`, must parse via `nostr.PubKeyFromHex`, exactly 64 hex
  chars). `Name`/`Icon`/`Description` are unconstrained, can be empty.
- `Policy` struct (`config.go:32-37`): `PublicRead`, `PublicWrite`, `PublicJoin`,
  `StripSignatures` — all `bool`, default `false`, no validation. **`PublicJoin` is the one field
  in this struct not read anywhere in `config.go`/`instance.go`** — it's only consumed by
  `ManagementStore.ValidateJoinRequest` (see `groups-and-management.md`).
- `Groups.Enabled`, `Push.Enabled`, `Management.Enabled` (`config.go:39-49`) — each gates the
  matching `instance.Groups/Push/Management.Enable(instance)` call in `makeInstance`
  (`instance.go:138-148`).
- `Blossom` struct (`config.go:51-56`): `Enabled`, `AuthenticatedRead`, `Adapter`, `S3`. `Adapter`
  defaults to `"local"` when empty — **as a mutation performed inside `Validate()`**
  (`config.go:121-123`) — and is restricted to `"local"`/`"s3"` (`config.go:149-164`).
- `BlossomS3Settings` (`config.go:73-80`): `Endpoint`, `Region`, `Bucket`, `AccessKey`,
  `SecretKey`, `KeyPrefix`. When `Adapter == "s3"`, `Bucket`/`Region`/`AccessKey`/`SecretKey` are
  required (`config.go:150-161`); `Endpoint`/`KeyPrefix` are always optional.
- `Livekit` struct (`config.go:58-62`): `ServerURL`, `APIKey`, `APISecret` — **zero validation
  anywhere in `Config.Validate()`**. Misconfiguration only surfaces at runtime when a LiveKit
  feature actually fires (`livekit.go` checks `APIKey == ""` and 404s per-request instead).
- `Roles map[string]Role` (`config.go:64`) — keyed by role name. **No validation of role names or
  pubkey hex format anywhere in `Validate()`.** See §5.
- Two unexported fields: `path string` (`config.go:67`, consumed by `Save()`) and `secret
  nostr.SecretKey` (`config.go:68`, the parsed form of `Secret`, populated only as a side effect
  of `Validate()`).

`Role` (`config.go:14-18`): `Pubkeys []string`, `CanInvite bool`, `CanManage bool`.

## 2. Config loading (`config.go:82-118`)

- `ConfigNameFromId(id string) string` (`config.go:82-84`) — trivially `id + ".toml"`, **no
  sanitization**. `ConfigPathFromName(name string) string` (`config.go:86-88`) —
  `filepath.Join(Env("CONFIG"), name)`, which lexically cleans `..` but doesn't prevent escaping
  the config dir. No allow-list check on `id`/`name` exists anywhere; the only thing standing
  between this and path traversal is that API callers derive `id` from a URL path segment
  (`r.PathValue("id")`), which Go's `net/http` mux decodes/normalizes first. Treat as a latent
  surface, not a proven exploit.
- `LoadConfigFromPath(path string) (*Config, error)` (`config.go:90-103`) — `toml.DecodeFile`
  (BurntSushi/toml). This decoder does **not** error on unknown/extra keys and does **not** error
  on missing required keys — a file that's just `host = "x"` decodes fine with everything else
  zero-valued; all "requiredness" is enforced afterward by `Validate()`. Sets `config.path = path`
  *before* validating, calls `Validate()`, returns `(nil, err)` on any failure — no
  partially-valid struct is ever handed back.
- `LoadConfigFromJson(path string, body []byte) (*Config, error)` (`config.go:105-118`) — same
  shape via `encoding/json.Unmarshal`, but `path` is an independent parameter unrelated to the
  JSON body — this is how `api.go`'s create/update handlers pre-assign the on-disk destination
  before the config is ever `Save()`d.
- Both loaders share `Validate()`'s side effects (defaulting `Blossom.Adapter`, caching
  `config.secret`) — relevant to PATCH, which re-derives `config.secret` from whatever `Secret`
  string survives the JSON-merge-patch, every time (`api.go:223-226`).

## 3. `Config.Validate()` order of checks (`config.go:120-167`)

1. Default `Blossom.Adapter` to `"local"` if empty — a **mutation**, unconditional, first.
2. `Host == ""` → `"host is required"`.
3. `Schema == ""` → `"schema is required"`.
4. `Schema` regex `^[a-z_][a-z0-9_]*$` → `"schema must contain only lowercase letters, numbers,
   and underscores"`. (Regex is `regexp.MustCompile`d fresh on every call, not cached — a minor
   recurring cost, not a bug.)
5. `nostr.SecretKeyFromHex(Secret)` → `"invalid secret key: %w"` on failure; on success, stashes
   the parsed key into `config.secret` — the *only* place that field is ever set. Hand-built
   `Config{}` literals (all four `_test.go` files do this) must set `secret` manually or every
   `Sign()`/`GetSelf()`/`IsSelf()` call is wrong.
6. `nostr.PubKeyFromHex(Info.Pubkey)` → `"invalid info.pubkey: %w"`.
7. Blossom S3 conditional block: if `Adapter == "s3"`, require all four S3 credential fields
   individually; else if `Adapter` isn't `"local"`/`"s3"` → `"invalid blossom adapter"`.
8. No validation of `Roles` contents at all.
9. `nil` if everything passed.

## 4. Exported functions `api.go` consumes

- `LoadConfigFromJson`/`LoadConfigFromPath` — create/update/patch flows and the
  duplicate-scan loop.
- `ConfigNameFromId`/`ConfigPathFromName` — translate the URL `{id}` into an on-disk filename.
- `Config.Save()` (`config.go:169-182`) — `os.Create` + `toml.NewEncoder`, **not atomic**: a
  crash mid-write, or the `fsnotify` watcher observing the file mid-write, can race (see §6).
- **Duplicate schema/host checking is NOT in `config.go` at all.** It's `api.go`'s own
  `checkDuplicateSchemaOrHost` — see `api-and-integrations.md` §1. `config.go`/`instance.go`/
  `lib.go`'s `Start()` perform **no** duplicate check whatsoever; only the REST API path does.

## 5. Roles model (`config.go:14-18, 222-272`)

- `GetAssignedRoles(pubkey) []Role` (`config.go:222-231`) is **dead code** — zero call sites
  outside its own definition, not exercised by any test. Note there is a *different*,
  unrelated `ManagementStore.GetAssignedRoles(pubkey) []string` in `management.go:388`
  that *is* used — same name, different receiver, different return type, different semantics.
  Easy to grep-confuse the two.
- `GetAllRoles(pubkey) []Role` (`config.go:233-244`): for the role literally keyed `"member"`,
  the role is **unconditionally included for every pubkey, without consulting `Pubkeys` at all**
  (`config.go:236-237`). For every other role name, standard `slices.Contains(role.Pubkeys,
  pubkey.Hex())`. **Practical consequence: `[roles.member].pubkeys` is functionally vestigial** —
  whatever you list there is never read; the member role applies to literally every pubkey purely
  because the map key equals `"member"`. Confirmed by `config_test.go:135-158` (empty `Pubkeys`
  under `member` still grants the role to any pubkey).
- A pubkey assigned to no role table, with no `[roles.member]` defined at all, gets an empty
  `GetAllRoles()` result → `CanInvite`/`CanManage` both `false` unless it's the owner or the
  relay's own key.
- `CanInvite`/`CanManage` (`config.go:246-258, 260-272`): short-circuit `true` if
  `IsOwner(pubkey) || IsSelf(pubkey)`, else OR together the matching boolean across every role
  from `GetAllRoles` — **permissions are additive/union across roles, never restrictive**.
  `IsOwner` compares against `Info.Pubkey`; `IsSelf` compares against the relay's own keypair
  derived from `Secret` — so the relay's own automated identity (used to sign generated invite
  events) always implicitly has full invite/manage rights.
- **Role pubkeys are never format-validated.** Two silent-failure consequences: (a) a malformed
  string in `Pubkeys` just never string-equals a real `pubkey.Hex()` — silent no-op, never an
  error; (b) in `makeInstance` (`instance.go:154-160`), the loop seeding the `ManagementStore`
  allow-list from `config.Roles` discards unparsable entries with no log — an operator who typos
  a pubkey in `[roles.admin].pubkeys` gets zero feedback.
- `GetOwner()` (`config.go:214-216`) uses `nostr.MustPubKeyFromHex`, which **panics** on
  invalid/empty input — safe only because `Validate()` already checked `Info.Pubkey` via the
  non-panicking form. Any hand-built `Config` (as all test files do) that skips setting
  `Info.Pubkey` correctly will panic the first time `GetOwner`/`IsOwner`/`CanInvite`/`CanManage`
  is called.

## 6. Gotchas, invariants, non-obvious behavior

- **Hot-reload drop-then-rebuild race (the most important finding here).** `lib.go`'s fsnotify
  handler (`lib.go:102-152`) always removes the existing instance from both
  `instancesByHost`/`instancesByName` *before* attempting to rebuild it (`lib.go:114-119`), for
  both `Write` and `Create` events. If the subsequent `MakeInstance(filename)` call fails
  (`lib.go:124-126`) — e.g. fsnotify fired mid-write of a non-atomic `Config.Save()`, or a
  hand-edited config now fails `Validate()` — **the virtual relay is left absent from both maps
  with no automatic rollback or retry.** It stays unreachable via `Dispatch`/`DispatchBySchema`
  until another filesystem event happens to arrive and parse successfully. There is no
  "keep serving the last-good config until the new one validates" semantics anywhere.
- **`Cleanup()` (`instance.go:165-168`) does exactly two things**:
  `instance.Relay.DisableExpirationManager()` and `instance.Events.Close()` (the latter a
  deliberate no-op — "never close the database, since it's a shared resource"). So `Cleanup()`'s
  real job is stopping the expiration-manager goroutine. `StartExpirationManager` is called
  unconditionally for every instance (`instance.go:108`) and launches a genuine background
  goroutine on an hourly ticker that closes over that specific instance's `QueryStored`/
  `DeleteEvent`. **If `Cleanup()` is ever skipped when discarding an `Instance`, this goroutine
  leaks indefinitely** — it keeps the whole `Instance`/`Config`/`EventStore` graph reachable and
  keeps periodically deleting expired events under that instance's schema prefix even though the
  instance is no longer dispatchable. `lib.go` is careful to call `Cleanup()` in every code path
  it controls (initial-load-but-inactive, reload-but-inactive, pre-rebuild teardown) — but see the
  next point for a path it does *not* control.
- **`makeInstance` fully builds the DB schema and wires all stores even when `Inactive`.**
  `Config.Inactive` is checked nowhere inside `makeInstance` — `instance.Events.Init()` runs
  unconditionally, as does the owner/role allow-list seeding. Only the *caller* checks
  `.Config.Inactive` after the fact and calls `Cleanup()`. Toggling `inactive` on a config that
  already has data doesn't create/drop tables — SQLite tables for a schema, once created, persist
  forever (there is no `DROP TABLE`/schema-teardown anywhere).
- **Schema name is validated as a safe SQL identifier exactly once** — the regex in `Validate()`.
  Every consumer of the schema for table naming (`Schema.Prefix`/`Schema.Render`, used pervasively
  in `events.go`'s raw SQL string templates) trusts this regex already ran; `Schema{Name:
  config.Schema}` is constructed directly with no re-validation. Because `text/template` splices
  `{{.Name}}` straight into raw SQL (not parameterized), **an unvalidated `Schema.Name` reaching
  this code is a straightforward SQL-injection vector into `CREATE TABLE`/`CREATE INDEX`.** The
  type system does not enforce that only validated schemas ever reach `Schema{Name: ...}` — it's
  safe today only because every real construction path goes through `Validate()` first.
- **Duplicate `host`/`schema` across config files dropped directly onto disk is never caught.**
  `lib.go`'s `Start()`/watcher loop performs zero conflict checking — only `api.go`'s REST path
  does (see `api-and-integrations.md`). Two manually-placed config files with the same `host`:
  the second clobbers the first in `instancesByHost` (`lib.go:83`, last-file-wins by
  `os.ReadDir`'s sort order); the first instance stays alive (tables created, goroutine running)
  but permanently unreachable via `Dispatch`, and is never `Cleanup()`'d — a leaked goroutine
  reachable purely through operator error. `DispatchBySchema` has the analogous hazard for
  duplicate `schema` with differing hosts: it resolves to whichever instance map iteration (an
  unordered Go map) happens to hit first — relevant because LiveKit's shared-webhook routing uses
  exactly this lookup.
- **`Config` mutation is not thread-safe at the struct level.** No mutex in `config.go` guards
  field reads/writes. Concurrency safety lives one layer up in `lib.go`'s `instancesMux
  sync.RWMutex`, which guards swap-out of whole `*Instance`/`*Config` pointers during
  watcher-driven reloads — but does **not** protect in-place mutation of a live `*Config` still
  being served. `Config.SetName`/`SetDescription`/`SetIcon` (`config.go:184-200`, invoked from
  NIP-86 handlers) mutate fields and `Save()` directly, concurrently with request-handling
  goroutines re-reading `config.Policy.*`/`config.Roles` live, per-request, with no
  synchronization.
- **Secret-key hex has asymmetric leniency vs. pubkey hex.** `nostr.SecretKeyFromHex` left-pads
  any input shorter than 64 hex chars before decoding — a truncated/typo'd secret silently
  produces a *different but still "valid"* keypair rather than an error. `nostr.PubKeyFromHex`
  (used for `Info.Pubkey`) requires exactly 64 chars, no padding. An empty-string secret is caught
  only indirectly (zero-pads to an all-zero key, which fails an explicit all-zero check) — not via
  a symmetric `"secret is required"` message the way `Host`/`Schema` get.

## 7. Test-derived gaps (`config_test.go`, `instance_test.go`, `lib_test.go`)

- `config_test.go` never exercises `LoadConfigFromPath`/`FromJson`'s error paths for missing
  `host`/`schema`/malformed secret directly — only the Blossom-adapter branch of `Validate()` gets
  a dedicated table-driven test, plus one full round-trip TOML-file test that also proves
  `[blossom.s3]` fields (including `secret_key`) survive a real disk-file parse **unredacted** —
  `LoadConfigFromPath` does not touch sensitive fields; they load into memory in cleartext exactly
  as written.
- All role/permission tests construct `Config{}` via struct literal and manually populate
  `secret`/`Info.Pubkey`/`Roles`, entirely **bypassing `Validate()`** — they can't catch
  regressions in the interaction between `Validate()`'s side effects and the permission methods.
- `instance_test.go`'s `createTestInstance()` helper builds a minimal hand-rolled `Instance` that
  **does not go through `MakeInstance`/`makeInstance` at all**, and leaves `instance.Blossom`,
  `instance.Groups`, `instance.Push` as `nil` — any test using this helper would nil-panic on a
  path touching those fields (e.g. `PreventBroadcast`/`OnRequest` reference
  `instance.Groups.IsGroupEvent`). **The two "real" wiring functions, `MakeInstance`/
  `makeInstance`, have no dedicated test at all** — nothing verifies that `Enabled` flags actually
  gate their `Enable()` calls, that NIP-11 info fields get copied, that routes get registered, or
  that `Cleanup()` correctly disables the expiration manager.
- `lib_test.go`'s only test, `TestDispatch_IgnoresInactiveInstances`, hand-populates
  `instancesByHost` directly rather than going through `Start()`/`MakeInstance` — so even
  `Dispatch`'s "real" integration path (scanning a config directory, invoking `MakeInstance`,
  populating the maps via `Start()`) has zero test coverage.
- `TestInstance_GenerateInviteEvent` confirms generated invite events are signed by the relay's
  own key (`Config.GetSelf()`), not the owner's — reinforcing the `IsSelf`/`IsOwner` split.
- No test in either file exercises concurrent access to `Config` or `Instance` (no goroutines),
  consistent with §6's observation that nothing guards in-place `Config` mutation.

## Sources

- `Config` struct and validation — `zooid/config.go:14-167`
- config loading + duplicate-check split — `zooid/config.go:82-118`, `zooid/api.go:110-134`
- roles model — `zooid/config.go:14-18,222-272`, `zooid/config_test.go:11-158`
- instance lifecycle, `Cleanup`, expiration manager — `zooid/instance.go:24-168`,
  `zooid/lib.go:60-153`
- hot-reload race — `zooid/lib.go:102-152`
- schema templating / SQL-injection latency — `zooid/schema.go:13-25`, `zooid/events.go:27-83`
- test gaps — `zooid/config_test.go`, `zooid/instance_test.go`, `zooid/lib_test.go`
