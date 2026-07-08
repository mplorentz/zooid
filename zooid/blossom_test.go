package zooid

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru"
)

func createTestBlossomInstance(t *testing.T) *Instance {
	t.Helper()

	setTestEnv("MEDIA", t.TempDir())

	config := &Config{Host: "test.com", secret: nostr.Generate()}
	config.Blossom.Adapter = "local"

	schema := &Schema{Name: "test_" + RandomString(8)}
	relay := khatru.NewRelay()
	events := &EventStore{Relay: relay, Config: config, Schema: schema}
	events.Init()

	instance := &Instance{
		Relay:      relay,
		Config:     config,
		Events:     events,
		Management: &ManagementStore{Config: config, Events: events},
	}
	instance.Blossom = &BlossomStore{Config: config, Events: events}
	instance.Blossom.Enable(instance)

	return instance
}

// Regression test for a fixed upstream bug: the vendored blossom server's
// DELETE handler used to dereference the parsed Authorization event without
// a nil-check whenever the header was missing or didn't start with
// "Nostr ", panicking on any unauthenticated DELETE request. It's fixed in
// the pinned nostrlib dependency now (handleDelete requires auth
// unconditionally), so this just confirms that end-to-end through zooid's
// own wiring rather than via a workaround in this package.
func TestBlossomStore_Enable_DeleteWithoutAuthHeaderDoesNotPanic(t *testing.T) {
	instance := createTestBlossomInstance(t)

	hash := strings.Repeat("a", 64)
	req := httptest.NewRequest(http.MethodDelete, "/"+hash, nil)
	w := httptest.NewRecorder()

	instance.Relay.Router().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("DELETE without Authorization header = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestBlossomStore_Enable_DeleteWithMalformedAuthHeaderDoesNotPanic(t *testing.T) {
	instance := createTestBlossomInstance(t)

	hash := strings.Repeat("a", 64)
	req := httptest.NewRequest(http.MethodDelete, "/"+hash, nil)
	req.Header.Set("Authorization", "Bearer not-nostr")
	w := httptest.NewRecorder()

	instance.Relay.Router().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("DELETE with malformed Authorization header = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}
