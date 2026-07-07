package zooid

import (
	"testing"

	"fiatjaf.com/nostr"
)

func TestGetGroupIDFromEvent(t *testing.T) {
	tests := []struct {
		name string
		kind nostr.Kind
		tags nostr.Tags
		want string
	}{
		{
			name: "with h tag",
			tags: nostr.Tags{{"h", "group123"}},
			want: "group123",
		},
		{
			name: "without h tag",
			tags: nostr.Tags{{"p", "pubkey123"}},
			want: "",
		},
		{
			name: "empty tags",
			tags: nostr.Tags{},
			want: "",
		},
		{
			name: "put-pins moderation event uses h tag",
			kind: GROUP_PUT_PINS,
			tags: nostr.Tags{{"h", "group123"}},
			want: "group123",
		},
		{
			name: "pins mirror event uses d tag",
			kind: GROUP_PINS,
			tags: nostr.Tags{{"d", "group123"}},
			want: "group123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := nostr.Event{Kind: tt.kind, Tags: tt.tags}
			result := GetGroupIDFromEvent(event)
			if result != tt.want {
				t.Errorf("GetGroupIDFromEvent() = %v, want %v", result, tt.want)
			}
		})
	}
}

func createTestGroupStore() *GroupStore {
	events := createTestEventStore()
	events.Init()

	management := &ManagementStore{
		Config: events.Config,
		Events: events,
	}

	return &GroupStore{
		Config:     events.Config,
		Events:     events,
		Management: management,
	}
}

func TestGroupStore_UpdatePins(t *testing.T) {
	groups := createTestGroupStore()

	eventID1 := nostr.Generate().Public().Hex()
	eventID2 := nostr.Generate().Public().Hex()

	putPins := nostr.Event{
		Kind:      GROUP_PUT_PINS,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"h", "group123"},
			{"e", eventID1},
			{"e", eventID2},
		},
	}

	if err := groups.UpdatePins(putPins); err != nil {
		t.Fatalf("UpdatePins() error = %v", err)
	}

	pinsEvent, found := func() (nostr.Event, bool) {
		filter := nostr.Filter{
			Kinds: []nostr.Kind{GROUP_PINS},
			Tags:  nostr.TagMap{"d": []string{"group123"}},
		}
		for evt := range groups.Events.QueryEvents(filter, 1) {
			return evt, true
		}
		return nostr.Event{}, false
	}()

	if !found {
		t.Fatal("UpdatePins() did not create a pins mirror event")
	}

	dTag := pinsEvent.Tags.Find("d")
	if dTag == nil || dTag[1] != "group123" {
		t.Errorf("pins event d tag = %v, want group123", dTag)
	}

	eTags := make([]string, 0)
	for tag := range pinsEvent.Tags.FindAll("e") {
		eTags = append(eTags, tag[1])
	}

	if len(eTags) != 2 || eTags[0] != eventID1 || eTags[1] != eventID2 {
		t.Errorf("pins event e tags = %v, want [%s, %s] in order", eTags, eventID1, eventID2)
	}

	// Submitting a new list should replace the old one, not accumulate
	putPinsAgain := nostr.Event{
		Kind:      GROUP_PUT_PINS,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"h", "group123"},
			{"e", eventID2},
		},
	}

	if err := groups.UpdatePins(putPinsAgain); err != nil {
		t.Fatalf("UpdatePins() second call error = %v", err)
	}

	filter := nostr.Filter{Kinds: []nostr.Kind{GROUP_PINS}, Tags: nostr.TagMap{"d": []string{"group123"}}}
	count := 0
	var latest nostr.Event
	for evt := range groups.Events.QueryEvents(filter, 0) {
		count++
		latest = evt
	}

	if count != 1 {
		t.Fatalf("expected exactly one pins event after replace, got %d", count)
	}

	eTags = make([]string, 0)
	for tag := range latest.Tags.FindAll("e") {
		eTags = append(eTags, tag[1])
	}

	if len(eTags) != 1 || eTags[0] != eventID2 {
		t.Errorf("pins event after replace = %v, want [%s]", eTags, eventID2)
	}
}

func TestGroupStore_CheckWrite_PutPins(t *testing.T) {
	groups := createTestGroupStore()
	groups.Config.Groups.Enabled = true

	adminSecret := nostr.Generate()
	memberSecret := nostr.Generate()

	groups.Config.Info.Pubkey = nostr.Generate().Public().Hex()
	groups.Config.Roles = map[string]Role{
		"admin": {Pubkeys: []string{adminSecret.Public().Hex()}, CanManage: true},
	}

	// Create the group so it's found
	groups.UpdateMetadata(nostr.Event{Kind: nostr.KindSimpleGroupEditMetadata, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"h", "group123"}}})

	putPinsFromAdmin := nostr.Event{
		Kind:   GROUP_PUT_PINS,
		PubKey: adminSecret.Public(),
		Tags:   nostr.Tags{{"h", "group123"}},
	}

	if msg := groups.CheckWrite(putPinsFromAdmin); msg != "" {
		t.Errorf("CheckWrite() for admin put-pins = %q, want empty", msg)
	}

	putPinsFromMember := nostr.Event{
		Kind:   GROUP_PUT_PINS,
		PubKey: memberSecret.Public(),
		Tags:   nostr.Tags{{"h", "group123"}},
	}

	if msg := groups.CheckWrite(putPinsFromMember); msg == "" {
		t.Error("CheckWrite() should reject put-pins from a non-admin")
	}

	directPinsWrite := nostr.Event{
		Kind:   GROUP_PINS,
		PubKey: adminSecret.Public(),
		Tags:   nostr.Tags{{"d", "group123"}},
	}

	if msg := groups.CheckWrite(directPinsWrite); msg == "" {
		t.Error("CheckWrite() should reject direct writes to the pins mirror event")
	}
}

func TestGroupStore_DeleteGroup_RemovesPins(t *testing.T) {
	groups := createTestGroupStore()

	putPins := nostr.Event{
		Kind:      GROUP_PUT_PINS,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"h", "group123"}, {"e", nostr.Generate().Public().Hex()}},
	}

	if err := groups.UpdatePins(putPins); err != nil {
		t.Fatalf("UpdatePins() error = %v", err)
	}

	groups.DeleteGroup("group123")

	filter := nostr.Filter{Kinds: []nostr.Kind{GROUP_PINS}, Tags: nostr.TagMap{"d": []string{"group123"}}}
	for range groups.Events.QueryEvents(filter, 0) {
		t.Error("DeleteGroup() should have removed the pins mirror event")
	}
}
