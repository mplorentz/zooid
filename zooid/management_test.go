package zooid

import (
	"slices"
	"strconv"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru"
)

func createTestManagementStore() *ManagementStore {
	config := &Config{
		Host:   "test.com",
		secret: nostr.Generate(),
	}
	config.Info.Pubkey = nostr.Generate().Public().Hex()
	schema := &Schema{Name: "test_" + RandomString(8)}
	relay := khatru.NewRelay()
	events := &EventStore{
		Relay:  relay,
		Config: config,
		Schema: schema,
	}
	events.Init()

	return &ManagementStore{
		Config: config,
		Events: events,
	}
}

func countRelayMembershipEvents(mgmt *ManagementStore, kind nostr.Kind) int {
	count := 0
	for range mgmt.Events.QueryEvents(nostr.Filter{Kinds: []nostr.Kind{kind}}, 0) {
		count++
	}

	return count
}

func TestManagementStore_MembershipChangesAreIdempotent(t *testing.T) {
	mgmt := createTestManagementStore()

	pubkey := nostr.Generate().Public()

	// Removing a pubkey that was never a member shouldn't record anything
	if err := mgmt.RemoveMember(pubkey); err != nil {
		t.Fatalf("RemoveMember() error = %v", err)
	}

	if count := countRelayMembershipEvents(mgmt, RELAY_REMOVE_MEMBER); count != 0 {
		t.Errorf("remove-member events = %d, want 0", count)
	}

	for i := 0; i < 2; i++ {
		if err := mgmt.AddMember(pubkey); err != nil {
			t.Fatalf("AddMember() call %d error = %v", i+1, err)
		}
	}

	if !mgmt.IsMember(pubkey) {
		t.Error("IsMember() = false, want true")
	}

	if count := countRelayMembershipEvents(mgmt, RELAY_ADD_MEMBER); count != 1 {
		t.Errorf("add-member events = %d, want 1", count)
	}

	for i := 0; i < 2; i++ {
		if err := mgmt.RemoveMember(pubkey); err != nil {
			t.Fatalf("RemoveMember() call %d error = %v", i+1, err)
		}
	}

	if mgmt.IsMember(pubkey) {
		t.Error("IsMember() = true, want false")
	}

	if count := countRelayMembershipEvents(mgmt, RELAY_REMOVE_MEMBER); count != 1 {
		t.Errorf("remove-member events = %d, want 1", count)
	}
}

func TestManagementStore_BanPubkey(t *testing.T) {
	mgmt := createTestManagementStore()

	pubkey := nostr.Generate().Public()
	reason := "spam"

	// Note: BanPubkey might return "duplicate event" error due to implementation
	// but the banning should still work
	mgmt.BanPubkey(pubkey, reason)

	// Test that pubkey is now banned
	if !mgmt.PubkeyIsBanned(pubkey) {
		t.Error("PubkeyIsBanned() should return true after banning")
	}

	// Test banned pubkey items
	bannedItems := mgmt.GetBannedPubkeyItems()
	itemFound := false
	for _, item := range bannedItems {
		if item.PubKey == pubkey && item.Reason == reason {
			itemFound = true
			break
		}
	}
	if !itemFound {
		t.Error("GetBannedPubkeyItems() should include banned pubkey with reason")
	}
}

func TestManagementStore_AllowPubkey(t *testing.T) {
	mgmt := createTestManagementStore()

	pubkey := nostr.Generate().Public()

	// Ban then allow
	mgmt.BanPubkey(pubkey, "test")

	if !mgmt.PubkeyIsBanned(pubkey) {
		t.Error("Setup: pubkey should be banned")
	}

	mgmt.AllowPubkey(pubkey)

	if mgmt.PubkeyIsBanned(pubkey) {
		t.Error("PubkeyIsBanned() should return false after allowing")
	}
}

func TestManagementStore_UnbanPubkey(t *testing.T) {
	mgmt := createTestManagementStore()

	pubkey := nostr.Generate().Public()

	mgmt.BanPubkey(pubkey, "test")

	if !mgmt.PubkeyIsBanned(pubkey) {
		t.Error("Setup: pubkey should be banned")
	}

	if err := mgmt.UnbanPubkey(pubkey, "appeal accepted"); err != nil {
		t.Fatalf("UnbanPubkey() should not return error: %v", err)
	}

	if mgmt.PubkeyIsBanned(pubkey) {
		t.Error("PubkeyIsBanned() should return false after unbanning")
	}
}

func TestManagementStore_UnallowPubkey(t *testing.T) {
	mgmt := createTestManagementStore()

	pubkey := nostr.Generate().Public()

	if err := mgmt.AllowPubkey(pubkey); err != nil {
		t.Fatalf("AllowPubkey() should not return error: %v", err)
	}

	if !mgmt.IsMember(pubkey) {
		t.Error("Setup: pubkey should be a member")
	}

	if err := mgmt.UnallowPubkey(pubkey, "membership revoked"); err != nil {
		t.Fatalf("UnallowPubkey() should not return error: %v", err)
	}

	if mgmt.IsMember(pubkey) {
		t.Error("IsMember() should return false after unallowing")
	}
}

func TestManagementStore_BanEvent(t *testing.T) {
	mgmt := createTestManagementStore()

	eventID := nostr.MustIDFromHex("1234567890123456789012345678901234567890123456789012345678901234")
	reason := "inappropriate"

	mgmt.BanEvent(eventID, reason)

	// Test that event is now banned
	if !mgmt.EventIsBanned(eventID) {
		t.Error("EventIsBanned() should return true after banning")
	}

	// Test banned event items
	bannedItems := mgmt.GetBannedEventItems()
	itemFound := false
	for _, item := range bannedItems {
		if item.ID == eventID && item.Reason == reason {
			itemFound = true
			break
		}
	}
	if !itemFound {
		t.Error("GetBannedEventItems() should include banned event with reason")
	}
}

func TestManagementStore_AllowEvent(t *testing.T) {
	mgmt := createTestManagementStore()

	eventID := nostr.MustIDFromHex("1234567890123456789012345678901234567890123456789012345678901234")

	// Ban then allow
	mgmt.BanEvent(eventID, "test")

	if !mgmt.EventIsBanned(eventID) {
		t.Error("Setup: event should be banned")
	}

	mgmt.AllowEvent(eventID, "unbanned")

	if mgmt.EventIsBanned(eventID) {
		t.Error("EventIsBanned() should return false after allowing")
	}
}

func roleTagValue(event nostr.Event, key string) string {
	tag := event.Tags.Find(key)
	if len(tag) < 2 {
		return ""
	}

	return tag[1]
}

func TestManagementStore_CreateRole(t *testing.T) {
	mgmt := createTestManagementStore()

	if err := mgmt.CreateRole("king", "King", "ruler of the relay", 37, 1); err != nil {
		t.Fatalf("CreateRole() error = %v", err)
	}

	event, ok := mgmt.GetRoleDefinition("king")
	if !ok {
		t.Fatal("GetRoleDefinition() should return the created role")
	}

	if event.Kind != RELAY_ROLE {
		t.Errorf("role event kind = %v, want %v", event.Kind, RELAY_ROLE)
	}

	if !HasTag(event.Tags, "-") {
		t.Error("role event should carry a NIP 70 \"-\" tag")
	}

	if got := roleTagValue(event, "d"); got != "king" {
		t.Errorf("d tag = %q, want %q", got, "king")
	}

	if got := roleTagValue(event, "label"); got != "King" {
		t.Errorf("label tag = %q, want %q", got, "King")
	}

	if got := roleTagValue(event, "description"); got != "ruler of the relay" {
		t.Errorf("description tag = %q, want %q", got, "ruler of the relay")
	}

	if got := event.Tags.Find("color"); !slices.Equal(got, nostr.Tag{"color", "37"}) {
		t.Errorf("color tag = %v, want %v", got, nostr.Tag{"color", "37"})
	}

	if got := roleTagValue(event, "order"); got != "1" {
		t.Errorf("order tag = %q, want %q", got, "1")
	}

	if event.PubKey != mgmt.Config.GetSelf() {
		t.Error("role event should be signed by the relay self key")
	}
}

func TestManagementStore_CreateRole_Duplicate(t *testing.T) {
	mgmt := createTestManagementStore()

	if err := mgmt.CreateRole("king", "King", "", 0, 0); err != nil {
		t.Fatalf("CreateRole() error = %v", err)
	}

	if err := mgmt.CreateRole("king", "King", "", 0, 0); err == nil {
		t.Error("CreateRole() should error when the role already exists")
	}
}

func TestManagementStore_CreateRole_OmitsEmptyAndZero(t *testing.T) {
	mgmt := createTestManagementStore()

	if err := mgmt.CreateRole("plain", "", "", 0, 0); err != nil {
		t.Fatalf("CreateRole() error = %v", err)
	}

	event, ok := mgmt.GetRoleDefinition("plain")
	if !ok {
		t.Fatal("GetRoleDefinition() should return the created role")
	}

	for _, key := range []string{"label", "description", "color", "order"} {
		if HasTag(event.Tags, key) {
			t.Errorf("role event should omit empty/zero %q tag", key)
		}
	}
}

func TestManagementStore_CreateRole_InvalidColor(t *testing.T) {
	mgmt := createTestManagementStore()

	if err := mgmt.CreateRole("king", "King", "", 400, 0); err == nil {
		t.Error("CreateRole() should error on out-of-range hue")
	}

	if err := mgmt.CreateRole("king", "King", "", -1, 0); err == nil {
		t.Error("CreateRole() should error on out-of-range hue")
	}

	if _, ok := mgmt.GetRoleDefinition("king"); ok {
		t.Error("invalid role should not have been stored")
	}
}

func TestManagementStore_EditRole(t *testing.T) {
	mgmt := createTestManagementStore()

	if err := mgmt.EditRole("king", "King", "", 0, 0); err == nil {
		t.Error("EditRole() should error when the role does not exist")
	}

	if err := mgmt.CreateRole("king", "King", "ruler", 10, 1); err != nil {
		t.Fatalf("CreateRole() error = %v", err)
	}

	if err := mgmt.EditRole("king", "Monarch", "the boss", 200, 2); err != nil {
		t.Fatalf("EditRole() error = %v", err)
	}

	event, ok := mgmt.GetRoleDefinition("king")
	if !ok {
		t.Fatal("GetRoleDefinition() should return the edited role")
	}

	if got := roleTagValue(event, "label"); got != "Monarch" {
		t.Errorf("label tag = %q, want %q", got, "Monarch")
	}

	if got := roleTagValue(event, "color"); got != "200" {
		t.Errorf("color tag = %q, want %q", got, "200")
	}

	// Editing replaces the definition, so there should only be a single role event.
	count := 0
	for range mgmt.Events.QueryEvents(nostr.Filter{Kinds: []nostr.Kind{RELAY_ROLE}}, 0) {
		count++
	}
	if count != 1 {
		t.Errorf("expected 1 role event after edit, got %d", count)
	}
}

func TestManagementStore_AssignAndUnassignRole(t *testing.T) {
	mgmt := createTestManagementStore()

	pubkey := nostr.Generate().Public()

	if err := mgmt.CreateRole("king", "King", "", 0, 0); err != nil {
		t.Fatalf("CreateRole() error = %v", err)
	}

	if err := mgmt.AssignRole(pubkey, "king"); err != nil {
		t.Fatalf("AssignRole() error = %v", err)
	}

	// Assigning a role implies membership.
	if !mgmt.IsMember(pubkey) {
		t.Error("AssignRole() should make the pubkey a member")
	}

	if roles := mgmt.GetAssignedRoles(pubkey); len(roles) != 1 || roles[0] != "king" {
		t.Errorf("GetAssignedRoles() = %v, want [king]", roles)
	}

	// Assignment is idempotent and must not duplicate the role.
	if err := mgmt.AssignRole(pubkey, "king"); err != nil {
		t.Fatalf("AssignRole() repeat error = %v", err)
	}

	if roles := mgmt.GetAssignedRoles(pubkey); len(roles) != 1 {
		t.Errorf("GetAssignedRoles() after repeat = %v, want one entry", roles)
	}

	if err := mgmt.UnassignRole(pubkey, "king"); err != nil {
		t.Fatalf("UnassignRole() error = %v", err)
	}

	if roles := mgmt.GetAssignedRoles(pubkey); len(roles) != 0 {
		t.Errorf("GetAssignedRoles() after unassign = %v, want empty", roles)
	}

	// Unassigning a role does not revoke membership.
	if !mgmt.IsMember(pubkey) {
		t.Error("UnassignRole() should leave the pubkey a member")
	}
}

func TestManagementStore_AssignRole_UnknownRole(t *testing.T) {
	mgmt := createTestManagementStore()

	pubkey := nostr.Generate().Public()

	if err := mgmt.AssignRole(pubkey, "ghost"); err == nil {
		t.Error("AssignRole() should error for an undefined role")
	}

	if mgmt.IsMember(pubkey) {
		t.Error("failed AssignRole() should not add membership")
	}
}

func TestManagementStore_DeleteRole(t *testing.T) {
	mgmt := createTestManagementStore()

	pubkey := nostr.Generate().Public()

	if err := mgmt.CreateRole("king", "King", "", 0, 0); err != nil {
		t.Fatalf("CreateRole() error = %v", err)
	}

	if err := mgmt.AssignRole(pubkey, "king"); err != nil {
		t.Fatalf("AssignRole() error = %v", err)
	}

	if err := mgmt.DeleteRole("king"); err != nil {
		t.Fatalf("DeleteRole() error = %v", err)
	}

	if _, ok := mgmt.GetRoleDefinition("king"); ok {
		t.Error("GetRoleDefinition() should return false after deletion")
	}

	// Deleting a role must strip dangling assignments from the members list.
	if roles := mgmt.GetAssignedRoles(pubkey); len(roles) != 0 {
		t.Errorf("GetAssignedRoles() after delete = %v, want empty", roles)
	}

	// The pubkey remains a member, just without the deleted role.
	if !mgmt.IsMember(pubkey) {
		t.Error("DeleteRole() should leave the pubkey a member")
	}
}

func TestManagementStore_DeleteRole_BroadcastsDeletion(t *testing.T) {
	mgmt := createTestManagementStore()

	if err := mgmt.CreateRole("king", "King", "", 0, 0); err != nil {
		t.Fatalf("CreateRole() error = %v", err)
	}

	role, ok := mgmt.GetRoleDefinition("king")
	if !ok {
		t.Fatal("GetRoleDefinition() should return the created role")
	}

	if err := mgmt.DeleteRole("king"); err != nil {
		t.Fatalf("DeleteRole() error = %v", err)
	}

	filter := nostr.Filter{Kinds: []nostr.Kind{nostr.KindDeletion}}

	var deletion *nostr.Event
	for event := range mgmt.Events.QueryEvents(filter, 1) {
		e := event
		deletion = &e
	}

	if deletion == nil {
		t.Fatal("DeleteRole() should store a deletion event")
	}

	address := nostr.EntityPointer{
		Kind:       RELAY_ROLE,
		PublicKey:  role.PubKey,
		Identifier: "king",
	}.AsTagReference()

	if tag := deletion.Tags.FindWithValue("e", role.ID.Hex()); tag == nil {
		t.Errorf("deletion event missing e tag for %s", role.ID.Hex())
	}

	if tag := deletion.Tags.FindWithValue("a", address); tag == nil {
		t.Errorf("deletion event missing a tag for %s", address)
	}

	if tag := deletion.Tags.FindWithValue("k", strconv.Itoa(RELAY_ROLE)); tag == nil {
		t.Errorf("deletion event missing k tag for kind %d", RELAY_ROLE)
	}
}

func TestManagementStore_SignEvent_AllowedKind(t *testing.T) {
	mgmt := createTestManagementStore()

	tags := nostr.Tags{nostr.Tag{"d", "test"}}

	event, err := mgmt.SignEvent(nostr.KindApplicationSpecificData, 0, tags, "hello")
	if err != nil {
		t.Fatalf("SignEvent() error = %v", err)
	}

	if event.PubKey != mgmt.Config.GetSelf() {
		t.Errorf("SignEvent() signed with %s, want relay key %s", event.PubKey, mgmt.Config.GetSelf())
	}

	if !event.VerifySignature() {
		t.Error("SignEvent() produced an invalid signature")
	}

	// A zero created_at must be replaced with the current time.
	if event.CreatedAt == 0 {
		t.Error("SignEvent() should default a missing created_at to the current time")
	}

	// SignEvent only signs; the caller publishes the event back to the relay itself.
	filter := nostr.Filter{Kinds: []nostr.Kind{nostr.KindApplicationSpecificData}, Tags: nostr.TagMap{"d": []string{"test"}}}

	for stored := range mgmt.Events.QueryEvents(filter, 1) {
		if stored.ID == event.ID {
			t.Error("SignEvent() should not store the signed event")
		}
	}
}

func TestManagementStore_SignEvent_RejectsOtherKinds(t *testing.T) {
	mgmt := createTestManagementStore()

	if _, err := mgmt.SignEvent(nostr.KindTextNote, 0, nil, ""); err == nil || err.Error() != "kind not allowed" {
		t.Errorf("SignEvent() error = %v, want \"kind not allowed\"", err)
	}
}

// The relay must not sign an event OnEvent would then refuse to accept, so the reserved
// "zooid/" d-tag namespace is rejected at sign time rather than at publish time.
func TestManagementStore_SignEvent_RejectsInternalEvents(t *testing.T) {
	mgmt := createTestManagementStore()

	tags := nostr.Tags{nostr.Tag{"d", BANNED_PUBKEYS}}

	_, err := mgmt.SignEvent(nostr.KindApplicationSpecificData, 0, tags, "")
	if err == nil || err.Error() != "d tag is reserved for internal use" {
		t.Errorf("SignEvent() error = %v, want \"d tag is reserved for internal use\"", err)
	}
}

func TestManagementStore_PubkeyIsBanned_NotBanned(t *testing.T) {
	mgmt := createTestManagementStore()

	pubkey := nostr.Generate().Public()

	if mgmt.PubkeyIsBanned(pubkey) {
		t.Error("PubkeyIsBanned() should return false for non-banned pubkey")
	}
}

func TestManagementStore_EventIsBanned_NotBanned(t *testing.T) {
	mgmt := createTestManagementStore()

	eventID := nostr.MustIDFromHex("abcdef1234567890123456789012345678901234567890123456789012345678")

	if mgmt.EventIsBanned(eventID) {
		t.Error("EventIsBanned() should return false for non-banned event")
	}
}

func TestManagementStore_CreateClaim(t *testing.T) {
	mgmt := createTestManagementStore()

	if err := mgmt.CreateClaim("welcome"); err != nil {
		t.Fatalf("CreateClaim() error = %v", err)
	}

	if !mgmt.ClaimExists("welcome") {
		t.Error("ClaimExists() should return true after CreateClaim()")
	}

	if claims := mgmt.GetClaims(); !slices.Contains(claims, "welcome") {
		t.Errorf("GetClaims() = %v, want to contain %q", claims, "welcome")
	}

	// The claim is stored as a RELAY_INVITE event so it can be redeemed later.
	filter := nostr.Filter{Kinds: []nostr.Kind{RELAY_INVITE}}

	var stored *nostr.Event
	for event := range mgmt.Events.QueryEvents(filter, 0) {
		if event.Tags.FindWithValue("claim", "welcome") != nil {
			e := event
			stored = &e
		}
	}

	if stored == nil {
		t.Fatal("CreateClaim() should store a RELAY_INVITE event carrying the claim")
	}

	if stored.PubKey != mgmt.Config.GetSelf() {
		t.Errorf("CreateClaim() invite signed with %s, want relay key %s", stored.PubKey, mgmt.Config.GetSelf())
	}
}

func TestManagementStore_CreateClaim_Idempotent(t *testing.T) {
	mgmt := createTestManagementStore()

	if err := mgmt.CreateClaim("dup"); err != nil {
		t.Fatalf("CreateClaim() error = %v", err)
	}

	if err := mgmt.CreateClaim("dup"); err != nil {
		t.Fatalf("CreateClaim() second call error = %v", err)
	}

	count := 0
	for _, claim := range mgmt.GetClaims() {
		if claim == "dup" {
			count++
		}
	}

	if count != 1 {
		t.Errorf("CreateClaim() created %d claims for %q, want 1", count, "dup")
	}
}

func TestManagementStore_CreateClaim_Empty(t *testing.T) {
	mgmt := createTestManagementStore()

	if err := mgmt.CreateClaim(""); err == nil {
		t.Error("CreateClaim(\"\") should return an error")
	}
}

func TestManagementStore_GetClaims_ReturnsAll(t *testing.T) {
	mgmt := createTestManagementStore()

	for _, claim := range []string{"alpha", "beta", "gamma"} {
		if err := mgmt.CreateClaim(claim); err != nil {
			t.Fatalf("CreateClaim(%q) error = %v", claim, err)
		}
	}

	claims := mgmt.GetClaims()

	for _, want := range []string{"alpha", "beta", "gamma"} {
		if !slices.Contains(claims, want) {
			t.Errorf("GetClaims() = %v, want to contain %q", claims, want)
		}
	}
}

func TestManagementStore_DeleteClaim(t *testing.T) {
	mgmt := createTestManagementStore()

	if err := mgmt.CreateClaim("keep"); err != nil {
		t.Fatalf("CreateClaim() error = %v", err)
	}

	if err := mgmt.CreateClaim("drop"); err != nil {
		t.Fatalf("CreateClaim() error = %v", err)
	}

	if err := mgmt.DeleteClaim("drop"); err != nil {
		t.Fatalf("DeleteClaim() error = %v", err)
	}

	if mgmt.ClaimExists("drop") {
		t.Error("ClaimExists() should return false after DeleteClaim()")
	}

	if !mgmt.ClaimExists("keep") {
		t.Error("DeleteClaim() should not remove unrelated claims")
	}
}

func TestManagementStore_DeleteClaim_RemovesAllMatching(t *testing.T) {
	mgmt := createTestManagementStore()

	// Two separate RELAY_INVITE events can share a claim (e.g. an admin-created claim plus an
	// auto-generated per-pubkey invite). DeleteClaim must remove every match.
	for i := 0; i < 2; i++ {
		event := nostr.Event{
			Kind:      RELAY_INVITE,
			CreatedAt: nostr.Timestamp(1000 + i),
			Tags: nostr.Tags{
				[]string{"claim", "shared"},
				[]string{"p", nostr.Generate().Public().Hex()},
			},
		}

		if err := mgmt.Events.SignAndStoreEvent(&event, false); err != nil {
			t.Fatalf("SignAndStoreEvent() error = %v", err)
		}
	}

	if err := mgmt.DeleteClaim("shared"); err != nil {
		t.Fatalf("DeleteClaim() error = %v", err)
	}

	remaining := 0
	for event := range mgmt.Events.QueryEvents(nostr.Filter{Kinds: []nostr.Kind{RELAY_INVITE}}, 0) {
		if event.Tags.FindWithValue("claim", "shared") != nil {
			remaining++
		}
	}

	if remaining != 0 {
		t.Errorf("DeleteClaim() left %d invite events, want 0", remaining)
	}
}

func TestManagementStore_CreateClaim_ValidatesJoinRequest(t *testing.T) {
	mgmt := createTestManagementStore()

	if err := mgmt.CreateClaim("secret-code"); err != nil {
		t.Fatalf("CreateClaim() error = %v", err)
	}

	join := nostr.Event{
		PubKey:    nostr.Generate().Public(),
		Kind:      RELAY_JOIN,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			[]string{"claim", "secret-code"},
		},
	}

	if reject, msg := mgmt.ValidateJoinRequest(join); reject {
		t.Errorf("ValidateJoinRequest() rejected a valid claim: %s", msg)
	}

	// A join carrying an unknown claim must still be rejected.
	badJoin := nostr.Event{
		PubKey:    nostr.Generate().Public(),
		Kind:      RELAY_JOIN,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			[]string{"claim", "wrong-code"},
		},
	}

	if reject, _ := mgmt.ValidateJoinRequest(badJoin); !reject {
		t.Error("ValidateJoinRequest() should reject an unknown claim")
	}
}

// Regression test for a bug in the vendored khatru NIP-86 dispatcher: its
// "listbannedevents" case nil-checks ManagementAPI.ListBannedEvents but then
// actually calls ManagementAPI.ListEventsNeedingModeration, so leaving the
// latter unset turns every "listbannedevents" call into a nil-function-value
// panic. Enable works around this by wiring the same handler to both fields.
// The vendored khatru NIP-86 dispatcher used to have a copy/paste bug where
// its "listbannedevents" case nil-checked ManagementAPI.ListBannedEvents but
// then actually called ManagementAPI.ListEventsNeedingModeration, panicking
// on a nil function value for any relay (like this one) that only wires up
// ListBannedEvents. That's fixed directly in the pinned nostrlib dependency
// now, so this just confirms Enable wires ListBannedEvents correctly -
// nothing in zooid needs to work around the dispatcher anymore.
func TestManagementStore_Enable_WiresListBannedEvents(t *testing.T) {
	mgmt := createTestManagementStore()
	instance := &Instance{
		Relay:      khatru.NewRelay(),
		Config:     mgmt.Config,
		Events:     mgmt.Events,
		Management: mgmt,
	}

	mgmt.Enable(instance)

	if instance.Relay.ManagementAPI.ListBannedEvents == nil {
		t.Fatal("Enable() should set ManagementAPI.ListBannedEvents")
	}

	eventID := nostr.MustIDFromHex("1234567890123456789012345678901234567890123456789012345678901234")
	if err := mgmt.BanEvent(eventID, "spam"); err != nil {
		t.Fatalf("BanEvent() error = %v", err)
	}

	items, err := instance.Relay.ManagementAPI.ListBannedEvents(t.Context())
	if err != nil {
		t.Fatalf("ListBannedEvents() error = %v", err)
	}

	if len(items) != 1 {
		t.Errorf("ListBannedEvents() returned %d items, want 1", len(items))
	}
}
