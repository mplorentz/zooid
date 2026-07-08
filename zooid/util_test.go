package zooid

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"fiatjaf.com/nostr"
)

func nip98Request(t *testing.T, method, url string, createdAt nostr.Timestamp, secret nostr.SecretKey) *http.Request {
	t.Helper()

	event := nostr.Event{
		Kind:      nostr.KindHTTPAuth,
		CreatedAt: createdAt,
		Tags: nostr.Tags{
			{"u", url},
			{"method", method},
		},
	}
	event.Sign(secret)

	jevt, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("failed to marshal auth event: %v", err)
	}

	req := httptest.NewRequest(method, url, nil)
	req.Header.Set("Authorization", "Nostr "+base64.StdEncoding.EncodeToString(jevt))

	return req
}

func TestValidateNIP98Auth_AcceptsFreshEvent(t *testing.T) {
	secret := nostr.Generate()
	url := "http://relay.example.com/some/path"

	req := nip98Request(t, http.MethodGet, url, nostr.Now(), secret)

	pubkey, err := validateNIP98Auth(req)
	if err != nil {
		t.Fatalf("validateNIP98Auth() error = %v", err)
	}

	if pubkey != secret.Public() {
		t.Error("validateNIP98Auth() returned the wrong pubkey")
	}
}

// Regression test: validateNIP98Auth used to accept a validly-signed auth
// event regardless of its age, meaning a captured/leaked "Authorization:
// Nostr ..." header for a given (url, method) pair would be replayable
// forever.
func TestValidateNIP98Auth_RejectsStaleEvent(t *testing.T) {
	secret := nostr.Generate()
	url := "http://relay.example.com/some/path"

	req := nip98Request(t, http.MethodGet, url, nostr.Now()-3600, secret)

	if _, err := validateNIP98Auth(req); err == nil {
		t.Error("validateNIP98Auth() should reject an hour-old auth event")
	}
}

func TestValidateNIP98Auth_RejectsFutureEvent(t *testing.T) {
	secret := nostr.Generate()
	url := "http://relay.example.com/some/path"

	req := nip98Request(t, http.MethodGet, url, nostr.Now()+3600, secret)

	if _, err := validateNIP98Auth(req); err == nil {
		t.Error("validateNIP98Auth() should reject an auth event timestamped an hour in the future")
	}
}
