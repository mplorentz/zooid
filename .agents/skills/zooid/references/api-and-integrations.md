# REST management API, Blossom, push, and LiveKit (deep detail)

This is the lookup-depth companion to the SKILL.md "REST API" and "integrations" sections. All of
it lives in `zooid/api.go`, `zooid/blossom.go`, `zooid/push.go`, `zooid/livekit.go`.

## 1. `api.go` — the JSON REST API for remote relay management

### Wiring

`NewAPIHandler()` (`api.go:21-47`) builds the whitelist from `API_WHITELIST` (comma-separated
pubkeys, trimmed, empty entries dropped), then registers routes on a stdlib `http.ServeMux` using
Go 1.22 method-prefixed patterns:

- `POST /relay/{id}` → `createRelay`
- `PUT /relay/{id}` → `putRelay`
- `PATCH /relay/{id}` → `patchRelay`
- `DELETE /relay/{id}` → `deleteRelay`
- `GET /relay/{id}/members` → `listRelayMembers`
- `POST /.well-known/nip29/livekit/webhook` → `livekitWebhook`, registered **without** the
  `api.auth` wrapper (`api.go:41-42`) — it verifies the LiveKit webhook signature itself.

The five relay-CRUD routes are wrapped in `api.auth(...)`. `APIHandler.ServeHTTP` just sets
`Content-Type: application/json` and delegates to the mux — unmatched paths → 404, registered path
with wrong method → 405 (Go 1.22 mux's automatic exact-method routing). `cmd/relay/main.go` only
mounts `APIHandler` at all if `API_HOST` is non-empty, dispatching purely on `r.Host == apiHost`
vs. falling through to per-tenant `zooid.Dispatch(r.Host)` otherwise — the "shared host" is just
an `http.Host` string match at the top-level handler.

### Auth order (`api.auth`, `api.go:49-62`)

1. `validateNIP98Auth(r)` (`util.go:159-220`) → **401** on any failure.
2. `api.whitelist[pubkey.Hex()]` membership → **403** on failure.
3. Only then does the real handler run.

NIP-98 validation strictly precedes the whitelist check, so an attacker without a valid nostr
keypair never learns whether an arbitrary pubkey is whitelisted (no oracle). `validateNIP98Auth`
checks, in order: `Authorization` header present → `Nostr <base64>` scheme/format → valid base64
→ valid event JSON → `event.Kind == KindHTTPAuth`(27235) → `event.VerifySignature()` → **`event.
CreatedAt` within a ±60s window of `nostr.Now()`** (`util.go`, a fixed `const nip98MaxClockSkew`)
→ `u` tag matches the exact expected URL (scheme upgraded to https only via `r.TLS != nil` or
`X-Forwarded-Proto: https`) → `method` tag case-insensitively matches `r.Method`. The timestamp
check used to not exist at all, meaning a captured `Authorization: Nostr <base64>` header for a
given `(url, method)` pair was replayable forever; it's now bounded to roughly a minute either
side of "now," matching common NIP-98 implementations, and applies to both the REST API and the
LiveKit token endpoint (they share this one helper).

### `configFromRequest`/`patchFromRequest` and `applyPatch`

Both cap the body via `http.MaxBytesReader(nil, r.Body, 1024*1024)` (1 MiB) — note the first arg
is `nil`, not `w`, so the usual "body too large" write-triggered abort doesn't fire; the overflow
just surfaces as a normal read error mapped to 400. `configFromRequest` delegates entirely to
`LoadConfigFromJson(path, body)` (parses + calls `Config.Validate()`).

**`applyPatch`** (`api.go:241-264`): round-trips the *current* `Config` through JSON into a
`map[string]interface{}`, deep-merges the incoming patch on top via `deepMerge`, re-marshals into
a **fresh** `Config` (deliberately not mutating the original until this succeeds), then manually
copies the two unexported fields that don't survive JSON round-tripping — `path` and `secret`
(`api.go:257-259`) — before swapping `*config = patched` in place. Subtlety: if the patch supplies
a new `"secret"` string, the merge sets `Secret` (exported), then line 258 stomps `patched.secret`
back to the *old* parsed key — but `config.Validate()` runs immediately after `applyPatch` returns
(`api.go:223`) and re-derives `secret` from `Secret` unconditionally, so the newly patched secret
does take effect after validation, just not from the `applyPatch` step itself.

**`deepMerge(base, patch map[string]interface{})`** (`api.go:266-288`) — a classic RFC 7396 "JSON
Merge Patch": start from a shallow copy of `base`; for each patch key: `nil` → `delete(result,
k)` (this is the null-removes-fields semantic); both `base[k]` and the patch value are
`map[string]interface{}` → recurse; patch value is a map but `base[k]` isn't → **full replace**
(patch maps only merge against existing object-typed fields, never against a missing/scalar
base value); otherwise → plain overwrite. **Arrays are never merged element-wise** — supplying an
array for any key always fully replaces it.

### Handler-by-handler behavior

- **`createRelay`** (`api.go:138-163`): `os.Stat(path)` exists → **409**; parse/validate body →
  **400** on failure; `checkDuplicateSchemaOrHost(config, "")` → **409**; `config.Save()` → **500**
  on failure; else **201**.
- **`putRelay`** (`api.go:167-193`): file must already exist (**404** if not); does **not** load
  the existing config first — the request body must be a complete config;
  `checkDuplicateSchemaOrHost(config, name)` excludes the relay's own filename so an unchanged
  schema/host doesn't conflict with itself; else **200**.
- **`patchRelay`** (`api.go:197-239`): **404** if missing → load current config from disk →
  parse patch body (**400** on bad JSON) → `applyPatch` (**400** on error) → `config.Validate()`
  (**400** on failure — this is how "patch removes a required field" produces 400: deleting
  `host` makes `Config.Host == ""` fail validation) → `checkDuplicateSchemaOrHost` (**409**) →
  `config.Save()` (**500**) → **200**.
- **`deleteRelay`** (`api.go:292-307`): **404** if missing, else `os.Remove` (**500** on failure)
  → **200**. This is purely a filesystem operation — it does **not** touch
  `instancesByName`/`instancesByHost` directly; live unloading is handled asynchronously by the
  `fsnotify` watcher reacting to the `Remove` event. There's a window where the API responds 200
  before the in-memory instance is actually torn down.
- **`listRelayMembers`** (`api.go:311-367`) → `resolveRelayMembers(name)`: if the relay is
  currently loaded (checked in `instancesByName` under `instancesMux.RLock()`), members come
  straight from the live `instance.Management.GetMembers()`. If not loaded — including for
  `Inactive` relays, which are absent from `instancesByName` — it falls back to a **cold** path:
  `LoadConfigFromPath` + a throwaway `EventStore`+`ManagementStore` pair, `Init()`'d fresh per
  request, no caching. Errors: `fs.ErrNotExist` → 404, else 500.

### `checkDuplicateSchemaOrHost` (`api.go:110-134`)

Lists all `*.toml` files in `Env("CONFIG")`, skips directories/the excluded filename/non-`.toml`
files, `LoadConfigFromPath`s each remaining one, compares `Schema` then `Host` against the
candidate. **If an existing sibling config fails to parse/validate, the error is silently
swallowed** (`if existing, err := LoadConfigFromPath(path); err == nil { ... }`) — a broken
config file is simply skipped, meaning duplicate detection has a blind spot against anything
currently failing to load. This check is **exclusively** invoked from these REST handlers — see
`config-and-instance.md` §6 for how manually-dropped config files bypass it entirely.

### LiveKit webhook routing at the API layer

`api.go:373-408` peeks at `probe.Room.Metadata` (the relay schema stamped on room creation) from
the raw body, resolves the owning `Instance` via `DispatchBySchema(schema)` (active instances
only), re-wraps the already-consumed body into a fresh reader, and delegates to that instance's
own `livekitWebhookHandler`, which independently re-verifies the LiveKit HMAC signature using
*that specific relay's* `api_key`/`api_secret` — a forged/mismatched schema claim cannot pass
because signature verification happens per-relay afterward. Full detail in §4.

## 2. `blossom.go` — Blossom media protocol

### Endpoint set

zooid wires four authorization hooks — `RejectUpload`, `RejectGet`, `RejectList`, `RejectDelete`
(`blossom.go:44-83`) — into a `*blossom.BlossomServer` from the vendored
`khatru/blossom` package. That vendor package's `New()` registers the actual HTTP surface: `PUT
/upload`, `HEAD /upload`, `GET /media` (a stub that just 307-redirects to `/upload` — not a real
BUD-05 endpoint), `PUT /mirror`, `GET /list/{pubkey}`, `HEAD /<sha256>[.ext]`, `GET
/<sha256>[.ext]`, `DELETE /<sha256>[.ext]`, `PUT /report`. zooid only supplies hooks for
**upload, get, list, delete** — mirror and report have no zooid-level authorization hook beyond
the library's own built-in `t`-tag/expiration checks (though `handleMirror` does call
`bs.RejectUpload` internally). NIP-11 gets `BUD-00, BUD-01, BUD-02, BUD-11` appended — no BUD-04
(mirror) or BUD-05 advertised despite mirror being wired up.

There used to be a zooid-side guard wrapping the router `blossom.New` installs, working around a
nil-pointer panic in the vendored `handleDelete` (see §5) — it's gone now that the fix landed
upstream in the pinned `nostrlib` dependency itself (`handleDelete` requires a well-formed
`Authorization` header unconditionally, matching `handleUpload`'s existing pattern). `blossom.go`
is otherwise unmodified from a bare `blossom.New(instance.Relay, ...)` call plus the four hooks
above.

### Authorization

All four hooks gate on `instance.Management.IsMember(auth.PubKey)` — plain relay membership, no
tie-in to `GroupStore` roles for blossom specifically. Upload additionally enforces a **10 MiB**
hard cap, returning `(true, "file too large", 413)`. Get is the only one config-gated: it
short-circuits to allow (`return false, "", 200`) whenever `!Config.Blossom.AuthenticatedRead` —
blobs are publicly readable by default; only `authenticated_read = true` requires membership.

### Adapter abstraction

Not a Go interface — a plain `switch bl.Config.Blossom.Adapter { case "local": ...; case "s3":
...; default: log.Fatalf }` (`blossom.go:34-45`) that populates three closures
(`StoreBlob`/`LoadBlob`/`DeleteBlob`) on the shared vendor struct. **An unrecognized adapter
string calls `log.Fatalf`** — process-fatal at instance-construction time, not a returned error.
`Config.Validate()` should make this unreachable (it rejects unknown adapter strings before an
`Instance` is ever built) via both the API path and the direct-disk-load path used by
`MakeInstance`/the fsnotify reload loop — but there's no regression test asserting this can never
be reached, and more generally: **any single tenant's config triggering a `log.Fatal` anywhere in
the reload path takes the entire multi-tenant process down**, not just that tenant (e.g.
`instance.go:128-130`'s "failed to initialize event store" is also fatal).

### Local filesystem layout

`${MEDIA}/<schema>/` (via `afero.NewOsFs()`, `MkdirAll(dir, 0755)`). Files stored **flat, named
exactly `<sha256>` with no extension** — the `ext` parameter is accepted but never used by the
local adapter; content-type is recovered later from the blob-descriptor event, not the filename.
Per-tenant isolation is purely the `Config.Schema` subdirectory name.

### S3 client setup

AWS SDK v2, explicit region + **static** credentials (`credentials.NewStaticCredentialsProvider`
— session-token field always empty, no temporary/STS creds support). If `S3.Endpoint` is set,
`o.BaseEndpoint` is overridden and `o.UsePathStyle = true` is forced (the standard pattern for
S3-compatible providers like MinIO/R2). Object key: `[<key_prefix>/]<schema>/<sha256>` — no
extension, tenant isolation via the schema path segment in a shared bucket. **`LoadBlob` fully
buffers the object into memory via `io.ReadAll`** before returning a `bytes.Reader` — no
streaming/range-request passthrough, while the local adapter returns a real `io.ReadSeeker` (open
file handle) that supports range requests via `http.ServeContent`. S3-backed relays lose HTTP
range support and pay a full-object memory cost per download.

### Content-hash verification

Not implemented in zooid's own code — entirely delegated to the vendor library. `handleUpload`
computes `sha256.Sum256` over the actually-received bytes and *that* computed hash becomes the
storage key; the client never gets to assert a filename, so upload content genuinely cannot be
mis-hashed. For `PUT /mirror`, the server downloads the URL, computes the hash itself, and
requires the authorization event's `x` tag to equal it — the authorization is hash-bound even
though the storage key is still server-computed.

## 3. `push.go` — NIP-9a-style push (not Web Push/VAPID, not APNs/FCM)

This is a **relay-to-arbitrary-HTTP-callback webhook** mechanism: clients publish a kind-**30390**
(`PUSH_SUBSCRIPTION`) parameterized-replaceable event describing a filter and an HTTP callback
URL; the relay POSTs a small JSON notification to that URL whenever a matching event is stored.
`PushManager.Enable` just builds a plain `*http.Client{Timeout: 10*time.Second}` — confirming raw
outbound HTTP POST, no third-party push SDK — and appends `"9a"` to `SupportedNIPs`.

### Subscription validation (`ValidatePushSubscription`, `push.go:36-108`, called from
`instance.OnEvent` whenever `event.Kind == PUSH_SUBSCRIPTION`)

Rejects unless **all** hold: non-empty `d` tag; a `relay` tag exactly equal to `"wss://" +
Config.Host + "/"`; ≥1 `filter` tag, each a parseable `nostr.Filter` JSON blob; any `ignore` tags
must also parse; **exactly one** `callback` tag, non-empty, parseable as an `http`/`https` URL
whose host isn't a literal loopback/private/link-local/unspecified/multicast address
(`isBlockedCallbackIP`, `push.go:252-255`, checked here only when the hostname is itself an IP
literal — see the dial-time guard below for the general case); the author must not already have
more than 10 existing `PUSH_SUBSCRIPTION` events (checked via `CountEvents` before the new one is
stored, so the 11th registration is rejected).

### Dispatch (`HandleEvent`, `push.go:110-196`, called from both `OnEventSaved` for persisted
events and `OnEphemeralEvent` for ephemeral ones — every stored *and* every ephemeral event flows
through push matching)

1. Skip if `!IsReadableEvent(event)` (excludes `RELAY_INVITE`, internal `zooid/`-prefixed
   app-data events, write-only kinds including `RELAY_JOIN`/`LEAVE`/`PUSH_SUBSCRIPTION` itself).
2. Iterate **every** stored `PUSH_SUBSCRIPTION` event (`QueryEvents(filter, 0)` — unbounded, O(n)
   subscriptions per event, no indexing by filter shape).
3. Skip self-notifications.
4. Group event → enforce the *subscriber's* NIP-29 read access via `Groups.CanRead` so a
   subscription can't leak group-restricted content to a non-member subscriber.
5. Require a `filter` tag match; skip if any `ignore` tag matches.
6. Build `PushPayload{ID, Relay}`, embedding the **full event** only if the subscription carries
   an `include_event` tag — otherwise the callback just gets an event ID + relay URL.
7. `go p.sendCallback(...)` — fire-and-forget per matching subscription, unbounded goroutine
   fan-out per incoming event, no worker pool/rate limit.

### Delivery + backoff (`sendCallback`, `push.go:198-234`)

Plain `client.Post`. HTTP 200 → clear that callback's error counter. HTTP 404 → treat as
"endpoint gone," **delete the subscription outright**, clear the counter. Any other outcome
(network error or non-200/404) → increment a per-callback-URL error counter (`errorCounts
map[string]int`, mutex-guarded); at **10 consecutive failures** the triggering subscription is
deleted. **The counter key is the callback URL string, not the subscription ID** — if multiple
subscriptions (even from different authors) share a callback URL, their failures/successes pool
into one shared counter, so one subscription's failures can cause an unrelated subscription
(sharing the endpoint) to be deleted once the shared count hits 10.

### Config gating

`Config.Push.Enabled` gates `instance.Push.Enable(instance)` in `makeInstance`, and separately
both `OnEventSaved`/`OnEphemeralEvent` guard the actual `Push.HandleEvent` call. **But
`ValidatePushSubscription` itself is not gated on `Push.Enabled`** — a relay with `push.enabled =
false` still *accepts and stores* kind-30390 events (as long as they pass validation), it just
never actually delivers notifications. A subtle asymmetry between write-acceptance and the
feature flag.

### SSRF guard on the delivery client (`Enable`, `push.go:259-291`)

The registration-time check above only catches a callback whose *hostname is itself a literal IP*
— it can't catch a hostname that resolves to a local/internal address later, possibly a different
one each time (DNS rebinding). `Enable` closes that gap by building the `*http.Client` with a
custom `Transport.DialContext` backed by a `net.Dialer{Control: ...}`: the `Control` hook fires
with the literal address about to be connected to, **after** DNS resolution and immediately before
the actual socket `connect()`, so it can't be bypassed by whatever the hostname resolves to at
delivery time. It refuses to dial if that address is loopback/private/link-local/unspecified/
multicast (the same `isBlockedCallbackIP` check used at registration). `TestPushManager_Enable_
ClientRefusesToDialLoopback` proves this end-to-end by pointing a real `httptest.Server` (which
listens on 127.0.0.1) at the client and asserting the POST fails.

## 4. `livekit.go` — audio/video calls

### Room creation + schema stamping

`ensureLivekitRoom` (`livekit.go:53-94`) is memoized process-wide via a
`map[string]bool` keyed on `serverURL + "'" + roomName` — a room is only created once per process
lifetime per (serverURL, roomName) pair. POSTs to LiveKit's Twirp RPC `livekit.RoomService/
CreateRoom`, authenticated with a server-scoped JWT (`RoomCreate/RoomList/RoomAdmin`, no room
restriction). **The created room's `metadata` field is set to the relay's schema string** —
"Use the relay's schema as room metadata so we can use the same livekit creds for multiple
relays" — enabling the shared-webhook routing in `api.go` §1. `roomName` itself is the NIP-29
**group id**: **one LiveKit room == one NIP-29 group's `h`/`d` id**. `200` and `409` (room already
exists) both count as success; anything else is a hard error.

### Token issuance (`livekitTokenHandler`, mounted at `GET
/.well-known/nip29/livekit/{groupId}` on **each tenant relay's own router**)

Order of checks: CORS headers set unconditionally; `OPTIONS` short-circuits; `cfg.APIKey == ""` →
**404** (feature disabled for this tenant); `validateNIP98Auth` → **401**;
`instance.Management.IsMember(pubkey)` → **403** if not a relay member; `Groups.GetMetadata
(groupId)` → **404** if the group doesn't exist; group metadata must carry a `"livekit"` tag →
**403** if absent (LiveKit must be explicitly opted into per-group); if metadata also carries a
`"restricted"` tag, additionally requires `Groups.HasAccess(groupId, pubkey)` → **403** otherwise.
Then `ensureLivekitRoom` (lazy creation on first join) and `generateLivekitToken` issues a
**participant** JWT scoped to `RoomJoin: true, Room: groupId`, identity `pubkey.Hex() + ":" +
RandomString(16)` (the random suffix disambiguates multiple simultaneous sessions for the same
pubkey; uses `math/rand`, not crypto-random — fine here, it's a de-dup suffix, not a secret).

### Webhook signature verification — two routes converge on one check

Two routes: **per-tenant** `POST /.well-known/nip29/livekit/webhook` on each `Instance`'s own
router → `instance.livekitWebhookHandler` directly; **shared/API-host**, same path on the
top-level `*APIHandler` mux → `APIHandler.livekitWebhook`, which peeks at `room.metadata`
*without* any signature check yet, resolves the tenant via `DispatchBySchema`, and forwards to
that tenant's *same* `instance.livekitWebhookHandler`. Actual verification happens there: builds
`auth.NewSimpleKeyProvider(cfg.APIKey, cfg.APISecret)` and calls
`webhook.ReceiveWebhookEvent(r, kp)` from LiveKit's own SDK — an HMAC-SHA256-via-signed-JWT
wrapper over the raw body's SHA-256 digest, validated by the library's own code, not any
hand-rolled comparison in zooid. Failure → **401**. This is what makes the shared-webhook trick
safe: even though the pre-verification peek at `room.metadata` could be forged, the per-tenant
handler re-verifies against *that specific tenant's* `api_secret`, so a forged schema claim just
routes to (and is rejected by) the wrong tenant's key. If a tenant's `APIKey`/`APISecret` are
empty, the handler 404s before ever calling `ReceiveWebhookEvent` — LiveKit is opt-in per relay.

After verification: `event.GetRoom()` must be present (400), `room.GetName()` (the `groupId`)
must be non-empty (400), the group must exist (404) and carry the `"livekit"` tag (403). Event
type switch: `EventRoomFinished` → publish presence with an **empty** participant list;
`EventParticipantJoined|Left|ConnectionAborted` → re-fetch the **authoritative current**
participant list from LiveKit via `fetchLivekitParticipants` (a fresh Twirp `ListParticipants`
call, room-scoped `RoomAdmin` JWT) rather than trusting the webhook payload's own participant
info, then publish that — avoiding races/ordering issues between rapid join/leave webhooks by
always re-querying ground truth. Any other event type → `204`, no-op.
`fetchLivekitParticipants` parses each participant's `identity` by splitting on the first `:` and
taking the hex prefix as the pubkey (matching the token-issuance identity format), silently
dropping/logging unparseable identities, de-duplicating via a `seen` set.

### The presence event itself

`publishLiveKitPresence` builds a **`KindSimpleGroupLiveKitParticipants`** event (resolves to kind
**39004**, addressable — not the vestigial `ROOM_PRESENCE`/10312 constant, see
`groups-and-management.md` §7), tags `d=groupId` plus one `participant=<hex>` tag per current
participant, signed via `instance.Events.SignAndStoreEvent(&event, true)`. Zooid hand-rolls this
shape itself rather than using the upstream `nip29` package's own equivalent helpers
(`ToLiveKitParticipantsEvent`/`MergeInLiveKitParticipantsEvent`), which exist in the vendored
dependency but aren't used here.

## 5. Cross-cutting security concerns

### Fixed

1. **Vendored Blossom `DELETE` handler nil-pointer dereference on missing `Authorization`
   header.** In the pinned dependency's `handleDelete`, `readAuthorization(r)` used to return
   `(nil, nil)` — no error — whenever the header was simply absent or didn't start with
   `"Nostr "`. `handleDelete` null-checked `auth` for the `t`-tag check but then unconditionally
   dereferenced `auth.Tags.FindWithValue(...)` for a later check — a nil `*nostr.Event` field
   access. Any unauthenticated `DELETE /<64-hex-char>` request to a Blossom-enabled tenant used to
   trigger a Go nil-pointer panic in that request's goroutine. **Fixed directly in the `nostrlib`
   repo itself** (`../nostrlib` relative to zooid, a sibling checkout of the same org's fork) —
   `handleDelete` now requires a well-formed `Authorization` header unconditionally and 401s
   otherwise, matching `handleUpload`'s existing pattern — and zooid's `go.mod` bumped to the
   commit that includes the fix. Zooid used to carry its own workaround (a router-wrapping guard
   in `blossom.go`) for this; it's been removed now that the real fix is upstream — see §2.
   Regression tests: `TestBlossomStore_Enable_DeleteWithoutAuthHeaderDoesNotPanic`/
   `_WithMalformedAuthHeaderDoesNotPanic` in zooid (now exercising the real vendored fix
   end-to-end rather than a local workaround), plus `TestHandleDelete_MissingAuthHeaderDoesNotPanic`/
   `_MalformedAuthHeaderDoesNotPanic` directly in `nostrlib/khatru/blossom/handlers_test.go`.
2. **NIP-98 auth timestamp/replay window.** Neither `validateNIP98Auth` nor `api.auth` used to
   inspect `event.CreatedAt` against wall-clock time, so a captured `Authorization: Nostr
   <base64>` header for a given `(url, method)` pair was valid forever and replayable indefinitely
   by anyone who intercepted/logged it, against both the REST API and the LiveKit token endpoint
   (same helper). Fixed with a ±60s window — see §1. Regression tests:
   `TestValidateNIP98Auth_RejectsStaleEvent`, `TestValidateNIP98Auth_RejectsFutureEvent`.
3. **SSRF via push callbacks.** `ValidatePushSubscription` used to only check that `callback`
   parsed as an `http`/`https` URL, with no check against loopback/RFC1918/link-local/cloud-
   metadata targets, and `sendCallback` would POST relay-controlled event data to whatever URL a
   member registered, from the relay's own network position, on every matching event until 10
   consecutive failures. Fixed with a registration-time literal-IP check plus a dial-time
   `net.Dialer.Control` guard that closes the DNS-rebinding gap the registration-time check alone
   can't cover — see §3. Regression tests:
   `TestPushManager_ValidatePushSubscription_RejectsLocalCallback`,
   `TestPushManager_Enable_ClientRefusesToDialLoopback`.
4. **NIP-86 `listbannedevents` nil-function-call panic** (vendored dependency dispatch bug) — see
   `groups-and-management.md` §5. `khatru/nip86.go`'s dispatcher nil-checked
   `ManagementAPI.ListBannedEvents` but then called `ManagementAPI.ListEventsNeedingModeration`
   instead, panicking on a nil function value for any relay wiring only the former. **Fixed
   directly in `nostrlib`** with a one-line change (the call now matches the nil-check) once it
   became clear the copy-paste bug was still present in the checkout. Zooid's earlier workaround
   (aliasing `ManagementAPI.ListEventsNeedingModeration` to the same handler as `ListBannedEvents`
   in `ManagementStore.Enable`) has been removed now that the dispatcher itself is correct.
   Regression tests: `TestHandleNIP86_ListBannedEvents` in `nostrlib/khatru/nip86_test.go`,
   `TestManagementStore_Enable_WiresListBannedEvents` in zooid.
5. **SSRF via Blossom's `PUT /mirror`.** The vendored `handleMirror` used to do a server-side
   `http.Get` against an arbitrary client-supplied URL with no restriction on destination, gated
   only by relay membership (via `RejectUpload`, which it calls internally — and that hook's
   signature, `ctx, auth, size, ext`, never receives the target URL at all, so there was no
   zooid-side hook capable of blocking the fetch by destination the way the push-callback dial
   guard does). **Fixed directly in `nostrlib`**, the same commit as item 1: `handleMirror` now
   validates the URL scheme and fetches through a new `mirrorHTTPClient`
   (`khatru/blossom/ssrf.go`) whose dialer refuses loopback/private/link-local/unspecified/
   multicast addresses at actual dial time (same `net.Dialer.Control` technique as the push
   fix in item 3, so it's DNS-rebinding-safe too). Regression tests:
   `TestMirrorHTTPClient_RefusesLoopback`, `TestIsBlockedMirrorIP` in
   `nostrlib/khatru/blossom/handlers_test.go`.

### Accepted tradeoffs, not bugs

6. **`log.Fatalf` on an unknown Blossom adapter takes down the whole multi-tenant process** —
   should be unreachable in practice given `Config.Validate()` runs on both the API and
   direct-disk-load paths and already rejects unknown adapter strings, but there's no regression
   test locking that invariant in, and more generally a single tenant's fatal error anywhere in
   the reload path (e.g. event-store init failure) kills every other tenant sharing the process.
   Not touched by this pass since it's a startup-time invariant already enforced one layer up, not
   a reachable runtime bug.
7. **Push callback error-counter keyed by URL, not subscription ID** — failures pool across
   unrelated subscriptions sharing a callback endpoint, causing cross-subscription deletion. Left
   as-is: shared callback usually implies the same operator/service, and de-keying this would be a
   behavior change beyond the scope of the audit's findings.
8. **`checkDuplicateSchemaOrHost` silently skips unparseable sibling configs** — an
   availability/data-integrity edge case, not a direct authz bypass (NIP-98 + whitelist still
   gates who can trigger it).
9. **S3 credentials and other secrets round-trip through the JSON API in cleartext** on
   `PUT`/`PATCH`/`POST`, with no redaction — not a bug per se (there's no "GET config" endpoint
   today), but worth remembering before adding one.

## 6. Test-derived behavior (`api_test.go`)

- **Env test-isolation trick**: `Env()` memoizes `os.Environ()` once via `sync.Once`, so tests
  can't `os.Setenv` after the first `Env()` call in the test binary — `setTestEnv` works around
  this by mutating the package-level `env` map directly after forcing initialization. This relies
  on the package's tests running **sequentially**; adding `t.Parallel()` anywhere in this package
  would silently corrupt it.
- NIP-98 negative-path coverage confirms every failure mode maps to 401 except whitelist
  rejection (403) — missing header, wrong scheme, invalid base64, wrong kind, invalid signature,
  missing `u`/`method` tag, all 401; valid-but-non-whitelisted pubkey, 403.
- Duplicate-schema/host tests confirm the self-exclusion (`checkDuplicateSchemaOrHost(config,
  name)`) works on update, while reusing a *different* relay's schema still 409s.
- **PATCH null-removal is exercised exactly once**, specifically to prove it's rejected when it
  breaks validation (`{"host": null}` → 400) — there's no test of a *successful* null-removal of a
  genuinely optional field (e.g. clearing `info.icon`), a coverage gap.
- Members-endpoint tests are the only place exercising both branches of `resolveRelayMembers` —
  the live-instance branch and the cold-fallback branch (seeding a `RELAY_MEMBERS` event directly
  into a throwaway `EventStore`) — effectively the only documentation of the cold-read path's
  expected behavior.
- Method-not-allowed vs. not-found tests confirm reliance on Go 1.22 mux semantics rather than a
  hand-rolled router: wrong method on a registered path → 405; no matching pattern at all
  (including `/relay/` with an empty `{id}`) → 404.
- The "valid full config" test round-trips every top-level config section through
  JSON→`Config`→TOML and asserts on raw TOML content strings — a living (if coarse) contract for
  the exact TOML key names `config.Save()` emits.

## Sources

- REST API wiring, auth, CRUD handlers, patch/merge — `zooid/api.go:1-408`
- Blossom hooks and adapters — `zooid/blossom.go` (the DELETE-guard workaround this file used to
  carry has been removed; see below)
- Push subscription validation, dispatch, delivery, SSRF guard — `zooid/push.go:1-295`
- LiveKit room/token/webhook — `zooid/livekit.go:1-307`
- NIP-98 helper (incl. timestamp window) shared across API + LiveKit token endpoint —
  `zooid/util.go:159-230`
- zooid regression tests — `zooid/api_test.go`, `zooid/blossom_test.go`, `zooid/push_test.go`,
  `zooid/util_test.go`, `zooid/management_test.go`
  (`TestManagementStore_Enable_WiresListBannedEvents`)
- upstream fixes + their regression tests — `../nostrlib/khatru/blossom/handlers.go`
  (`handleDelete` auth requirement, `handleMirror` scheme validation),
  `../nostrlib/khatru/blossom/ssrf.go` (`mirrorHTTPClient`),
  `../nostrlib/khatru/blossom/handlers_test.go`; `../nostrlib/khatru/nip86.go` (`listbannedevents`
  dispatch fix), `../nostrlib/khatru/nip86_test.go`; commits `84df705` (Blossom) and `9d8949d`
  (nip86 dispatch) on `gitea.coracle.social/coracle/nostrlib` (`master`)
