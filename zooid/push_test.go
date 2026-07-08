package zooid

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru"
)

func createTestPushManager() *PushManager {
	config := &Config{Host: "test.com", secret: nostr.Generate()}
	schema := &Schema{Name: "test_" + RandomString(8)}
	events := &EventStore{Config: config, Schema: schema}
	events.Init()

	return &PushManager{Config: config, Events: events}
}

func pushSubscriptionEvent(secret nostr.SecretKey, host, callback string) nostr.Event {
	event := nostr.Event{
		Kind:      PUSH_SUBSCRIPTION,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"d", "sub1"},
			{"relay", "wss://" + host + "/"},
			{"filter", `{"kinds":[1]}`},
			{"callback", callback},
		},
	}
	event.Sign(secret)
	return event
}

// Regression test: ValidatePushSubscription only checked that the callback
// parsed as an http(s) URL, with no check at all against internal/local
// addresses - a member could register a callback pointing at loopback,
// RFC1918/link-local ranges, or a cloud metadata endpoint, and the relay
// would dutifully POST event data there from its own network position.
func TestPushManager_ValidatePushSubscription_RejectsLocalCallback(t *testing.T) {
	p := createTestPushManager()
	secret := nostr.Generate()

	localCallbacks := []string{
		"http://127.0.0.1/hook",
		"http://[::1]/hook",
		"http://169.254.169.254/latest/meta-data",
		"http://0.0.0.0/hook",
		"http://10.0.0.5/hook",
	}

	for _, callback := range localCallbacks {
		event := pushSubscriptionEvent(secret, p.Config.Host, callback)
		reject, msg := p.ValidatePushSubscription(event)
		if !reject {
			t.Errorf("ValidatePushSubscription() callback=%q should be rejected, was accepted", callback)
		}
		if msg == "" {
			t.Errorf("ValidatePushSubscription() callback=%q rejection should include a message", callback)
		}
	}
}

func TestPushManager_ValidatePushSubscription_AcceptsPublicCallback(t *testing.T) {
	p := createTestPushManager()
	secret := nostr.Generate()

	event := pushSubscriptionEvent(secret, p.Config.Host, "https://example.com/hook")
	reject, msg := p.ValidatePushSubscription(event)
	if reject {
		t.Errorf("ValidatePushSubscription() should accept a public callback, was rejected: %s", msg)
	}
}

// Regression test for the dial-time guard: even if a callback hostname
// resolves to a public address at registration time (or isn't a literal IP
// at all, so the registration-time check in ValidatePushSubscription can't
// catch it), the http.Client built in Enable must still refuse to actually
// connect to a loopback/internal address at delivery time.
func TestPushManager_Enable_ClientRefusesToDialLoopback(t *testing.T) {
	p := createTestPushManager()
	p.Enable(&Instance{Relay: khatru.NewRelay()})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, err := p.client.Post(srv.URL, "application/json", strings.NewReader("{}"))
	if err == nil {
		t.Error("push client should refuse to dial a loopback callback URL, but the request succeeded")
	}
}
