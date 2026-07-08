# NIP-29 groups and NIP-86 management (deep detail)

This is the lookup-depth companion to the SKILL.md "authorization model" section. It covers the
**four separate, easily-conflated authorization systems** in this codebase, the invite/claim
flow, the NIP-86 method dispatch, and the ban mechanics. All of it lives in `zooid/groups.go`,
`zooid/management.go`, cross-referenced against `zooid/instance.go` and `zooid/config.go`.

## 1. Four authorization systems — do not conflate them

**(a) Config-driven static roles** (`config.go:14-18,20-69,222-272`). `Config.Roles
map[string]Role` comes from the `[roles.*]` TOML table; deploy-time config, not an event. A role
literally named `"member"` applies to **every pubkey unconditionally** regardless of whether that
pubkey is tracked anywhere as a relay member (see `config-and-instance.md` §5). `CanInvite`/
`CanManage` fold in `IsOwner`/`IsSelf` plus a union across matching roles.

**(b) Relay-wide dynamic membership** (`ManagementStore`, `management.go:155-235`). Backed by two
mechanisms together: an append-only audit trail of `RELAY_ADD_MEMBER`(8000)/
`RELAY_REMOVE_MEMBER`(8001) events (one row per action), and a single canonical **replaceable**
`RELAY_MEMBERS`(13534) event carrying one `["member", pubkey_hex, ...roleIDs]` tag per member —
role IDs appended as extra tag elements (`management.go:388-396,427-449`). `IsMember`/
`GetMembers` read **only** the canonical list, never the audit trail. `GetAdmins()`
(`management.go:135-149`) is **not** derived from this list at all — it's `config.GetOwner()` +
every pubkey in a config role with `CanManage: true`. Admin status is 100% config-driven; there
is no way to grant it dynamically at runtime.

**(c) Dynamic role labels** (`management.go:237-474`, kind `RELAY_ROLE`=33534, addressable).
`CreateRole`/`EditRole`/`DeleteRole`/`AssignRole`/`UnassignRole` manage purely **cosmetic**
metadata — `label`, `description`, HSL `color` tag, `order` — for client UI (Flotilla role
badges), stored as one addressable event per role id plus role-id tags appended onto the
`RELAY_MEMBERS` member tag. **`AssignRole` grants zero permissions** — it is never consulted by
`CanInvite`/`CanManage`/`IsAdmin` anywhere. It only guarantees membership as a side effect
(comment: "a role is meaningless without membership"). Do not assume `assignrole` via NIP-86
changes what a pubkey can *do*.

**(d) Per-group membership** (`GroupStore`, `groups.go:157-230`) — a fourth, entirely separate
system keyed by the `h` group id, using standard NIP-29 `KindSimpleGroupPutUser`/`RemoveUser`
events, resolved by replaying history (`IsMember`, `groups.go:185-205`, returns on the first —
i.e. most-recent — matching event) and mirrored into a `KindSimpleGroupMembers` snapshot
(`UpdateMembersList`, `groups.go:232-249`). **`GroupStore.IsAdmin(h, pubkey)` and `GetAdmins(h)`
ignore the `h` parameter entirely** (`groups.go:130-136`) — both just delegate to
`g.Management.IsAdmin`/`GetAdmins()`, the same *global* relay admin set. There is no such thing as
a per-group admin distinct from the global relay admin here — `UpdateAdminsList(h)` publishes the
same global admin list under every group's `d` tag. A real limitation relative to what the NIP-29
spec implies.

`GroupStore.HasAccess(h, pubkey)` (`groups.go:253-255`) is the union of all relevant systems:
`Config.CanManage(pubkey) || g.IsAdmin(h, pubkey) || g.IsMember(h, pubkey)`. `IsAdmin(h,...)` is
fully subsumed by the first clause (since `GetAdmins()` ⊆ everyone `CanManage` already covers) —
dead weight, harmless.

## 2. Invite codes / "claims" (28934 / 28935 / 28936)

Two shapes of invite share one event kind, `RELAY_INVITE`=28935 (ephemeral kind-range — see the
"ephemeral-range kinds used non-ephemerally" note below):

- **Admin-named claims** — `ManagementStore.CreateClaim(claim)` (`management.go:517-537`) stores
  a bare `RELAY_INVITE` event with just `["claim", code]`, signed by the relay key,
  `broadcast=false`. Idempotent (checks `ClaimExists` first).
- **Auto-generated per-inviter claims** — `Instance.GenerateInviteEvent(pubkey)`
  (`instance.go:208-234`) fires from inside `QueryStored` (`instance.go:305-307`) whenever an
  authenticated, `Config.CanInvite`-authorized pubkey subscribes to a filter containing kind
  28935. See `event-store.md` §5 for a real bug here: the existence-check filter uses tag key
  `"#p"` instead of `"p"`, so it's effectively unfiltered and can return the wrong pubkey's
  invite entirely.

**Redemption** — `ManagementStore.ValidateJoinRequest` (`management.go:665-689`), invoked from
`Instance.OnEvent` for `RELAY_JOIN`=28934 (`instance.go:357-359`) **before** any other
write-permission check:
1. already a member → accept (idempotent re-join).
2. banned → reject `"invalid: you have been banned from this relay"`.
3. `Policy.PublicJoin` → accept unconditionally, no claim required.
4. else require a `"claim"` tag; reject if missing or unrecognized; accept if `ClaimExists`
   matches.

**Claims are reusable, not single-use.** `ValidateJoinRequest` never deletes/consumes the claim,
and the only post-acceptance side effect (`Instance.OnEphemeralEvent`, `instance.go:442-449`) is
`Management.AddMember`/`RemoveMember`. A single `claim` string can be redeemed by unlimited
distinct pubkeys until an admin calls `deleteclaim`. Only NIP-86 `createclaim` (gated by the
blanket `CanManage` check, §5) can admin-create a named claim — there's no `CanInvite`-only path
for that; the auto-generated per-pubkey path is the only one gated by `CanInvite`.

`DeleteClaim` (`management.go:539-563`) matches purely on the `claim` tag value and removes
**every** matching `RELAY_INVITE` event — deliberately covers both an admin-created claim and any
auto-generated invite sharing the same code string (confirmed by
`TestManagementStore_DeleteClaim_RemovesAllMatching`).

`instance.go:91` unconditionally advertises `"43"` in `Relay.Info.SupportedNIPs` **even when
`config.Management.Enabled` is false** — that append happens in base `makeInstance`, not inside
`ManagementStore.Enable`. A relay with claims/management disabled still claims support for it in
its NIP-11 doc. (Note: NIP-43 isn't a merged/official NIP number as far as this audit can
corroborate — appears to be this team's informal numbering for the nostr-protocol/nips#1079
draft.)

## 3. Access policies (`[policy]`) at the Khatru hook level

All in `instance.go`, in this exact call order per lifecycle stage:

- **Connect** — `OnConnect` (`instance.go:238-240`) unconditionally issues a NIP-42 AUTH
  challenge. Auth is always requested, though not always required to read.
- **Read, subscription setup** — `OnRequest` (`instance.go:275-291`): (1) no auth at all →
  reject `"auth-required"`; (2) `!Policy.PublicRead && !CanManage && !IsMember` → reject
  `"restricted: you are not a member of this relay"`; (3) `Policy.PublicRead &&
  PubkeyIsBanned` → reject `"restricted: you have been banned"`. Ban is only explicitly checked
  on the `PublicRead` branch because on the private branch, `BanPubkey` already forces
  `RemoveMember`, so `!IsMember` alone already excludes banned pubkeys.
- **Read, per-result filtering** — `QueryStored` (`instance.go:293-340`). Internal calls
  (`khatru.IsInternalCall`, used by expiration/deletion machinery) bypass all filtering. External
  queries: synthesize the caller's own invite event if applicable (§2), then for non-`CanManage`
  callers filter out `!IsReadableEvent` events, `PUSH_SUBSCRIPTION` events not owned by the
  caller, and group events the caller can't read via `Groups.CanRead`. `StripSignature`
  (`instance.go:172-179`) zeroes the `sig` field on every returned event when
  `Policy.StripSignatures` is true and the requester isn't `CanManage`.
- **Write** — `OnEvent` (`instance.go:344-392`), in order: (1) `AllowRecipientEvent` short-circuit
  for zap receipts/gift wraps addressed to a manager or member; (2) require auth **and**
  `pubkey == event.PubKey` (no publishing on behalf of others); (3) `RELAY_JOIN` → delegate
  entirely to `ValidateJoinRequest`, return; (4) `PUSH_SUBSCRIPTION` → delegate to
  `Push.ValidatePushSubscription`, return; (5) `!Policy.PublicWrite && !CanManage && !IsMember` →
  reject; (6) `Policy.PublicWrite && PubkeyIsBanned` → reject; (7) reject `IsInternalEvent`/
  `IsReadOnlyEvent` kinds outright; (8) group event → run `Groups.CheckWrite`; (9) reject if
  `EventIsBanned`.
- **Broadcast to already-open subscriptions** — `PreventBroadcast` (`instance.go:242-258`), a
  **separate** gate from `OnRequest`/`QueryStored`, evaluated per live event against every
  currently-authenticated pubkey on a websocket. Allows if any authed key is `CanManage`, or the
  event is a group event that key `CanRead`s, or it's the key's own `PUSH_SUBSCRIPTION`;
  otherwise falls through to `!IsReadableEvent(event)`. **This fallthrough never re-checks
  `PubkeyIsBanned` or general relay membership for non-group events** — on a `PublicRead=false`
  relay, a pubkey with an already-open subscription before being banned/removed keeps receiving
  broadcasts of ordinary (non-group) readable events; only *new* `OnRequest`/`QueryStored` calls
  are blocked. Narrow but real.

`Policy.PublicJoin` only affects `ValidateJoinRequest` — it has no direct effect on
`OnRequest`/`OnEvent`'s membership gates.

## 4. Khatru integration surface

Implemented on `Instance`, wired in `makeInstance` (`instance.go:95-104`): `OnConnect`,
`PreventBroadcast`, `StoreEvent`, `ReplaceEvent`, `DeleteEvent`, `OnRequest`, `QueryStored`,
`OnEvent`, `OnEventSaved`, `OnEphemeralEvent`. `GroupStore`/`ManagementStore` don't implement
Khatru hooks themselves — they expose `Enable(instance *Instance)` methods
(`groups.go:366-368`, `management.go:693-789`) called conditionally from `makeInstance` based on
`config.Groups.Enabled`/`config.Management.Enabled`. `GroupStore.Enable` only appends `"29"` to
`SupportedNIPs`; all actual group logic (`CanRead`, `CheckWrite`, `IsGroupEvent`) is invoked
directly from `Instance`'s hooks, not via any Khatru-level hook `GroupStore` registers itself.
`ManagementStore.Enable` populates the whole `khatru.RelayManagementAPI` struct (§5) plus its
`OnAPICall` gate.

Kind-range semantics that matter throughout (from the vendored `fiatjaf.com/nostr` /
`gitea.coracle.social/Coracle/nostrlib` fork, per `go.mod`'s replace directive): `IsRegular`
<10000 (excl. 0,3), `IsReplaceable` ==0||==3||[10000,20000), `IsEphemeral` [20000,30000),
`IsAddressable` [30000,40000). Khatru's core pipeline special-cases ephemeral kinds: for
`RELAY_JOIN`/`RELAY_LEAVE`/`PUSH_SUBSCRIPTION` (all ephemeral-range), khatru calls `OnEvent` then
`OnEphemeralEvent` and **never** calls `StoreEvent`/`ReplaceEvent`/`OnEventSaved` — these are
never persisted via the normal ingestion path; only zooid's own direct `SignAndStoreEvent` calls
persist rows for kinds in this range.

## 5. NIP-86 management API — exact dispatch

Decoding/dispatch lives in the vendored `khatru/nip86.go` (`HandleNIP86`): parses a NIP-98-style
`Authorization: Nostr <base64 event>` header (verifies sig, `u`-tag URL match, payload hash of
the body, a 30s freshness window), decodes the JSON body via `nip86.DecodeRequest` (string method
name → typed params struct), sets the caller's pubkey into context, calls
`rl.ManagementAPI.OnAPICall(ctx, mp)` if set, then dispatches via a type switch to the matching
`ManagementAPI.<Method>` closure. `supportedmethods` is answered via reflection over non-nil
struct fields (lower-cased field name = method name). **This used to have a reflection quirk**:
the exclusion list checked for a field named `"rejectapicall"`, which doesn't exist in this struct
version, so zooid's own `OnAPICall` gate was reported as a bogus supported method `"onapicall"`.
Fixed upstream in `../nostrlib` (commit `7cbf5a9`, "Fix bogus supported methods being returned
from nip 86") to exclude the correct field names (`"onapicall"` and `"generic"`) — no zooid-side
change needed, it just came along with the dependency bump for the `listbannedevents` fix.

**Authorization gate** (`management.go:693-706`): every method call, whatever it is, requires an
authed pubkey **and** `Config.CanManage(pubkey)`, else `"blocked: only relay admins can manage
this relay."`.

Zooid registers exactly these handlers (`management.go:708-788`): `changerelayname`,
`changerelaydescription`, `changerelayicon`, `banpubkey`, `unbanpubkey`, `allowpubkey`,
`unallowpubkey`, `listbannedpubkeys`, `listallowedpubkeys`, `banevent`, `allowevent`,
`listbannedevents`, `createrole`, `editrole`, `deleterole`, `assignrole`, `unassignrole`,
`listclaims`, `createclaim`, `deleteclaim`, `signevent`.

**Not implemented** — falls to the vendored `default` branch, always returns `"method '<x>' not
known"` since `ManagementAPI.Generic` is never set: `listallowedevents`, `allowkind`,
`disallowkind`, `listallowedkinds`, `listdisallowedkinds`, `blockip`, `unblockip`,
`listblockedips`, `stats`, `grantadmin`, `revokeadmin`. `listeventsneedingmoderation` *is*
a recognized method name (`nip86.DecodeRequest` knows it, so it doesn't fall to `default`), but
since zooid never wires `ManagementAPI.ListEventsNeedingModeration`, it answers `"method
listeventsneedingmoderation not supported"` via the vendor's own nil-check — zooid doesn't
distinguish "needs moderation" from "already banned" as separate concepts, and (unlike an earlier
version of this fix) no longer aliases it to `listbannedevents` either.

Results: mutating methods return `resp.Result = true` on success or `resp.Error = err.Error()`;
list methods return the slice directly as `resp.Result`.

**Fixed upstream — a vendored-library bug used to break `listbannedevents` end-to-end.** In
`khatru/nip86.go` (pinned dependency), the `listbannedevents` case checked
`rl.ManagementAPI.ListBannedEvents == nil` but then *called*
`rl.ManagementAPI.ListEventsNeedingModeration(ctx)` instead — a copy/paste bug in the vendor code.
Zooid used to set `ListBannedEvents` (`management.go`) but never `ListEventsNeedingModeration`, so
the nil check passed and the subsequent call invoked a **nil function value**, panicking that
request's handler goroutine — any admin UI (e.g. Flotilla's relay-management panel) calling
`listbannedevents` would see a hung/broken connection. This was fixed **directly in `nostrlib`**
(a sibling checkout at `../nostrlib`, same org's fork) with a one-line change — the call now
matches the nil-check, i.e. `rl.ManagementAPI.ListBannedEvents(ctx)` — once zooid's `go.mod` was
bumped to the commit with the fix. Zooid used to carry a workaround for this in
`ManagementStore.Enable` (aliasing `ManagementAPI.ListEventsNeedingModeration` to the same handler
as `ListBannedEvents`, so whichever field the buggy dispatch actually called would hit real logic)
— that workaround has been removed now that the dispatcher itself is correct.

Regression tests: `TestHandleNIP86_ListBannedEvents` in `nostrlib/khatru/nip86_test.go` (builds a
real `*khatru.Relay` with only `ManagementAPI.ListBannedEvents` wired — deliberately leaving
`ListEventsNeedingModeration` nil, the exact configuration that used to panic — and drives a full
signed `listbannedevents` NIP-86 request through `HandleNIP86`); `TestManagementStore_Enable_
WiresListBannedEvents` in zooid (confirms `Enable` wires `ManagementAPI.ListBannedEvents`
correctly — a plain wiring check now, not a workaround-specific regression test, since the
dispatcher bug this used to guard against no longer exists in the dependency zooid builds
against).

**Reason parameters are silently discarded on several methods.** `nip86.UnbanPubKey`/
`AllowPubKey`/`UnallowPubKey` structs carry a caller-supplied `Reason` string, but zooid's wrapper
closures drop it before calling into `ManagementStore`: `UnbanPubKey`
(`management.go:722-724→626-628`) calls `RemoveBannedPubkey(pubkey)` with no reason param at all;
`AllowPubKey`/`UnallowPubKey` similarly call reason-less methods. By contrast `BanPubkey`/
`BanEvent` **do** persist the reason. Asymmetric audit trail: you can see why something was
banned, never why it was un-banned or allowed.

## 6. Banned pubkeys/events

Despite the constants being named `BANNED_PUBKEYS`/`BANNED_EVENTS` and a code comment literally
calling them "the banned pubkeys list" (`management.go:16-19`), **these are not keys in the
literal `kv` SQLite table** (`kv.go` — a completely separate, unused-here abstraction). They are
`"d"`-tag identifiers (`"zooid/banned_pubkeys"`, `"zooid/banned_events"`, `util.go:25-26`) for
**NIP-78** `KindApplicationSpecificData`(30078, addressable) events, fetched/created via
`EventStore.GetOrCreateApplicationSpecificData` and persisted through the normal `ReplaceEvent`
path like any other addressable event. `IsInternalEvent` (`util.go:29-39`) marks any
`KindApplicationSpecificData` event whose `d` tag starts with `"zooid/"` as internal, and
`IsReadableEvent` hides it from `QueryStored`'s client-facing filtering and from `OnEvent`'s write
path (rejecting any client attempt to directly forge/overwrite these events) — only server-side
code can mutate the ban lists.

List format: a single event per list, each ban stored as one tag — `["banned", pubkey_hex,
reason]` for pubkeys, `["event", id_hex, reason]` for events. `AddBannedPubkey` is idempotent
(checked via `FindWithValue` first); `BanEvent` doesn't check for an existing entry before
appending — a minor asymmetry, harmless in practice. `BanPubkey` (`management.go:606-624`)
composes: remove from members list (aborting with `"Can't remove permanent admins from relay."`
if the target is a config-admin), add to the ban list, then hard-delete **every event ever
authored by that pubkey**. `BanEvent` hard-deletes the event *and* records the ban (so a
re-submission of the same event id is independently rejected by `EventIsBanned` even though the
row itself is gone). Enforcement: `PubkeyIsBanned` is checked conditionally in `OnRequest`/
`OnEvent` (§3); `EventIsBanned` is checked unconditionally at the tail of `OnEvent`.

## 7. Presence (kind 10312) — dead code, don't trust the constant

`ROOM_PRESENCE = 10312` (`util.go:18`) is defined but **never referenced anywhere else in the
package**. Presence is actually implemented in `livekit.go` via the NIP-29 metadata kind
`KindSimpleGroupLiveKitParticipants` (which resolves to kind **39004**, addressable) through
`publishLiveKitPresence` — see `api-and-integrations.md` §4. Treat `ROOM_PRESENCE`/10312 as
vestigial; the real wire kind for room presence is 39004.

## 8. Gotchas / invariants

- **Read-modify-write races, no locking.** There is exactly one `sync.RWMutex` in the whole
  package guarding the instance registry (`lib.go:16`), plus two unrelated mutexes in
  `livekit.go:21` and `push.go:25` — nothing guards `GetOrCreateRelayMembersList`/
  `GetOrCreateApplicationSpecificData` + mutate + `SignAndStoreEvent`. Since `RELAY_MEMBERS` is
  replaceable and the ban lists are addressable, `ReplaceEvent` does its own unprotected
  read-then-delete-then-insert across separate SQL statements. Two concurrent `AddMember`/
  `AssignRole`/`RemoveMember`/`BanPubkey` calls (e.g. two simultaneous NIP-86 calls, or a join
  racing an admin action) can both read the same starting members-list snapshot and the second
  write silently clobbers the first's addition/removal — a classic lost-update. This is the
  concrete mechanism behind "concurrent invite redemption double-spend": two pubkeys redeeming
  the same reusable claim concurrently both call `AddMember` and race exactly this way. The
  `RELAY_ADD_MEMBER` audit-trail kind is itself append-only and race-free, but nothing currently
  reconciles it against a possibly-stale `RELAY_MEMBERS` snapshot.
- **`GenerateInviteEvent`'s check-then-create runs inside a read path (`QueryStored`)** with the
  same unlocked race — overlapping subscriptions from the same inviter before the first insert
  commits can mint two distinct per-inviter invite codes.
- **Self-removal invariant (commit `983d045`).** The relay's own signing key
  (`config.GetSelf()`) used to be added to the members list at startup. It's deliberately **not**
  a "member" anymore (so it won't show up in member lists / Flotilla UI); it's kept privileged
  everywhere via `Config.CanManage` (which special-cases `IsSelf()`) instead of via membership —
  `AllowRecipientEvent`, `OnRequest`, and `OnEvent` were each patched to add a `CanManage` branch.
  Post-fix invariant: **"self" is authorized everywhere via `CanManage`, never via
  `ManagementStore.IsMember`**, and is absent from `GetMembers()`/`listallowedpubkeys` output.
- **Role color is a plain hue int, not HSL — and this changed twice upstream.** The vendored
  `nip86` package went `Color int` (hue) → `Color{Hue, Saturation, Lightness}` (a full HSL struct,
  briefly) → back to a plain `Hue int` on `CreateRole`/`EditRole`. Zooid's `CreateRole`/`EditRole`
  (`management.go`) and `buildRoleEvent` take `hue int` directly and match the current upstream
  shape: `hue == 0` is treated as "not set" (same ambiguity as `order` — the nip86 wire format
  can't distinguish an omitted value from an explicit zero) and produces no `color` tag; otherwise
  it renders `["color", "<hue>"]` after validating `0 <= hue <= 360`. If `management.go` ever fails
  to compile against a newer nostrlib with an "undefined: nip86.Color" (or similar) error, check
  `nip86/methods.go`'s `CreateRole`/`EditRole` struct definitions first — this API has moved more
  than once.
- **Ephemeral-range kinds used non-ephemerally, deliberately.** `RELAY_JOIN`/`RELAY_INVITE`/
  `RELAY_LEAVE` fall in nostr's 20000–29999 "ephemeral" range, which per NIP-01 convention relays
  shouldn't persist. Zooid deliberately persists `RELAY_INVITE` (via direct
  `SignAndStoreEvent`, bypassing khatru's ephemeral fast-path) so claims survive restarts and are
  queryable — an intentional convention override, not a bug, but worth flagging to anyone
  assuming standard ephemeral semantics apply uniformly.
- **Closed groups: join requests always land as events, but membership isn't auto-granted.**
  `CheckWrite` (`groups.go:341-347`) unconditionally accepts a `KindSimpleGroupJoinRequest` as
  long as the sender isn't already a member — the `"closed"` tag gate is only checked for *other*
  write kinds. Actual admission only happens in `OnEventSaved`
  (`instance.go:397-404`), and only `if ok && !meta.Tags.Has("closed")`. For closed groups, join
  requests land (visible to admins) but membership requires a separate admin
  `KindSimpleGroupPutUser` moderation event.
- **Two idioms for the same tag-presence check.** `groups.go` uses its own `HasTag(tags, key)`
  (`util.go:144-151`) while `instance.go:400` uses the upstream `nostr.Tags.Has(...)` method for
  the identical `"closed"` check — presumably equivalent, just inconsistent.
- **NIP-86 is never advertised in NIP-11.** `SupportedNIPs` gets `"29"`, `"43"` (unconditional),
  `"9a"` (conditional), and Blossom's `"BUD-*"` strings — but nothing ever appends `"86"` even
  though the management API is fully wired.

## 9. Test-derived behavior

- `TestGroupStore_CheckWrite_PutPins` proves `KindSimpleGroupPutPins`(9010, a zooid-local
  extension kind not in upstream `nip29`) requires `Config.CanManage`, and that a **direct** write
  of the mirror kind `KindSimpleGroupPins`(39005) is always rejected — clients must go through
  `9010`, never write `39005` directly.
- `TestGroupStore_UpdatePins` confirms pins are a full-replace (not accumulate) mirror.
- `TestManagementStore_CreateRole_InvalidColor` confirms out-of-range hues (`<0` or `>360`) are
  rejected and the role isn't stored; `TestManagementStore_CreateRole_OmitsEmptyAndZero` confirms
  `hue == 0` produces no `color` tag at all (the same omitted-vs-zero ambiguity as `order`).
- `TestManagementStore_DeleteRole`/`_BroadcastsDeletion` show `DeleteRole` does double duty:
  hard-deletes the role-definition row *and* broadcasts a standard NIP-09 kind-5 deletion event
  (with `e`, `a`, `k` tags) for federated caches, and scrubs the role id out of every member's
  `RELAY_MEMBERS` role-tag list — membership itself is untouched.
- `TestManagementStore_AssignRole_UnknownRole` confirms `AssignRole` is fail-closed: assigning a
  nonexistent role id errors out *before* calling `AddMember`.
- `TestManagementStore_SignEvent_RejectsOtherKinds` pins the exact allow-list for the
  relay-signs-on-behalf-of-admin feature: `KindApplicationSpecificData`(30078),
  `KindDeletion`(5), `30067`, `39067` only.
- `TestManagementStore_CreateClaim_ValidatesJoinRequest` is the one test exercising the full
  claim→join wiring end-to-end, but does **not** test whether the claim is consumed afterward —
  the "reusable by design" behavior is only verifiable by reading `management.go:665-689`
  directly.
- Both `groups_test.go` and `management_test.go` construct their stores directly against fresh
  SQLite schemas rather than through `MakeInstance` — the `makeInstance`-time bootstrapping
  (owner/role pubkeys auto-allowed, `instance.go:152-160`) is effectively only
  integration-tested, or untested, at the `Instance` level.

## Sources

- role/membership systems — `zooid/config.go:14-18,222-272`, `zooid/management.go:135-235`,
  `zooid/groups.go:130-136,157-255`
- invite/claim flow — `zooid/management.go:484-563,606-689`, `zooid/instance.go:208-234,357-359`
- policy hooks — `zooid/instance.go:172-392`
- khatru wiring — `zooid/instance.go:95-104,138-148`
- NIP-86 dispatch + vendor bugs — `zooid/management.go:693-788`, vendored
  `khatru/nip86.go:58-443`
- ban mechanics — `zooid/management.go:49-131,606-628`, `zooid/util.go:18-39`
- presence dead-code note — `zooid/util.go:18`, `zooid/livekit.go:294-307`
- `listbannedevents` dispatch-bug fix (upstream) + regression tests —
  `../nostrlib/khatru/nip86.go`, `../nostrlib/khatru/nip86_test.go`,
  `zooid/management_test.go` (`TestManagementStore_Enable_WiresListBannedEvents`)
