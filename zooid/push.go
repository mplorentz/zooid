package zooid

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"syscall"
	"time"

	"fiatjaf.com/nostr"
)

// Struct definition

type PushManager struct {
	Config       *Config
	Events       *EventStore
	Management   *ManagementStore
	Groups       *GroupStore
	client       *http.Client
	errorCounts  map[string]int // tracks consecutive errors per callback URL
	errorCountMu sync.Mutex     // protects errorCounts map
}

type PushPayload struct {
	ID    string       `json:"id"`
	Relay string       `json:"relay"`
	Event *nostr.Event `json:"event,omitempty"`
}

// Handlers

func (p *PushManager) ValidatePushSubscription(event nostr.Event) (reject bool, msg string) {
	if event.Tags.GetD() == "" {
		return true, "invalid: missing or empty d tag"
	}

	relayTag := event.Tags.Find("relay")
	if len(relayTag) < 2 {
		return true, "invalid: missing relay tag"
	}

	expected := nostr.NormalizeURL("wss://" + p.Config.Host)
	if nostr.NormalizeURL(relayTag[1]) != expected {
		return true, "invalid: relay tag does not match this relay's URL, expected " + expected
	}

	filterTags := slices.Collect(event.Tags.FindAll("filter"))
	if len(filterTags) == 0 {
		return true, "invalid: at least one filter tag is required"
	}

	for _, filterTag := range filterTags {
		if len(filterTag) < 2 {
			return true, "invalid: filter tag is malformed"
		}

		var filter nostr.Filter
		if err := json.Unmarshal([]byte(filterTag[1]), &filter); err != nil {
			return true, "invalid: filter tag contains invalid JSON: " + err.Error()
		}
	}

	for ignoreTag := range event.Tags.FindAll("ignore") {
		if len(ignoreTag) < 2 {
			return true, "invalid: ignore tag is malformed"
		}

		var filter nostr.Filter
		if err := json.Unmarshal([]byte(ignoreTag[1]), &filter); err != nil {
			return true, "invalid: ignore tag contains invalid JSON: " + err.Error()
		}
	}

	callbackTags := slices.Collect(event.Tags.FindAll("callback"))

	if len(callbackTags) < 1 {
		return true, "invalid: missing callback tag"
	}

	if len(callbackTags) > 1 {
		return true, "invalid: too many callback tags"
	}

	for _, callbackTag := range callbackTags {
		if len(callbackTag) < 2 || callbackTag[1] == "" {
			return true, "invalid: empty callback tag"
		}

		callbackURL := callbackTag[1]
		parsedURL, err := url.Parse(callbackURL)
		if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
			return true, "invalid: callback must be a valid HTTP or HTTPS URL"
		}

		// Reject callbacks that are obviously internal/local right away, for
		// clearer feedback. This isn't the actual SSRF defense - a hostname can
		// still resolve to an internal address later, possibly differently each
		// time (DNS rebinding) - the dial-time check in Enable is what actually
		// enforces this; this just fails fast for the common literal-IP case.
		if ip := net.ParseIP(parsedURL.Hostname()); ip != nil && isBlockedCallbackIP(ip) {
			return true, "invalid: callback must not point to a local or internal address"
		}
	}

	filter := nostr.Filter{
		Kinds:   []nostr.Kind{PUSH_SUBSCRIPTION},
		Authors: []nostr.PubKey{event.PubKey},
	}

	count, err := p.Events.CountEvents(filter)
	if err != nil {
		return true, "internal: failed to query database"
	}

	if count > 10 {
		return true, "invalid: too many subscriptions registered"
	}

	return false, ""
}

func (p *PushManager) HandleEvent(event nostr.Event) {
	if !IsReadableEvent(event) {
		log.Printf("[push] event %s kind=%d not readable; skipping", event.ID.Hex()[:8], event.Kind)
		return
	}

	log.Printf("[push] evaluating event %s kind=%d for push subscribers", event.ID.Hex()[:8], event.Kind)

	filter := nostr.Filter{
		Kinds: []nostr.Kind{PUSH_SUBSCRIPTION},
	}

	matchedSubscribers := 0
	for subscriptionEvent := range p.Events.QueryEvents(filter, 0) {
		matchedSubscribers++
		if event.PubKey == subscriptionEvent.PubKey {
			continue
		}

		if p.Groups.IsGroupEvent(event) && !p.Groups.CanRead(subscriptionEvent.PubKey, event) {
			log.Printf("[push] subscriber %s cannot read group event kind=%d; skipping", subscriptionEvent.PubKey.Hex()[:8], event.Kind)
			continue
		}

		filterTags := subscriptionEvent.Tags.FindAll("filter")
		matched := false
		for filterTag := range filterTags {
			if len(filterTag) < 2 {
				continue
			}

			var filter nostr.Filter
			if err := json.Unmarshal([]byte(filterTag[1]), &filter); err != nil {
				log.Printf("[push] subscription %s has malformed filter tag; skipping", subscriptionEvent.ID.Hex()[:8])
				continue
			}

			if filter.Matches(event) {
				matched = true
				break
			}
		}

		if !matched {
			log.Printf("[push] subscription %s (sub %s) filters did not match kind=%d; skipping", subscriptionEvent.ID.Hex()[:8], subscriptionEvent.PubKey.Hex()[:8], event.Kind)
			continue
		}

		ignoreTags := subscriptionEvent.Tags.FindAll("ignore")
		ignored := false
		for ignoreTag := range ignoreTags {
			if len(ignoreTag) < 2 {
				continue
			}

			var ignore nostr.Filter
			if err := json.Unmarshal([]byte(ignoreTag[1]), &ignore); err != nil {
				continue
			}

			if ignore.Matches(event) {
				ignored = true
				break
			}
		}

		if ignored {
			log.Printf("[push] subscription %s ignored event kind=%d; skipping", subscriptionEvent.ID.Hex()[:8], event.Kind)
			continue
		}

		callbackTag := subscriptionEvent.Tags.Find("callback")

		if callbackTag == nil || len(callbackTag) < 2 {
			continue
		}

		callback := callbackTag[1]

		payload := PushPayload{
			ID:    event.ID.Hex(),
			Relay: "wss://" + p.Config.Host + "/",
		}

		if subscriptionEvent.Tags.Find("include_event") != nil {
			payload.Event = &event
		}

		payloadBytes, err := json.Marshal(payload)
		if err != nil {
			continue
		}

		go p.sendCallback(subscriptionEvent.ID, callback, payloadBytes)
	}

	if matchedSubscribers == 0 {
		log.Printf("[push] no PUSH_SUBSCRIPTION events found in store; event kind=%d not pushed", event.Kind)
	} else {
		log.Printf("[push] scanned %d PUSH_SUBSCRIPTION events for event kind=%d", matchedSubscribers, event.Kind)
	}
}

func (p *PushManager) sendCallback(subscriptionID nostr.ID, callback string, payloadBytes []byte) {
	resp, err := p.client.Post(callback, "application/json", bytes.NewReader(payloadBytes))
	if resp != nil {
		defer resp.Body.Close()
	}

	incrementError := func() (count int) {
		p.errorCountMu.Lock()
		p.errorCounts[callback]++
		count = p.errorCounts[callback]
		p.errorCountMu.Unlock()

		return count
	}

	clearError := func() {
		p.errorCountMu.Lock()
		delete(p.errorCounts, callback)
		p.errorCountMu.Unlock()
	}

	if err == nil && resp.StatusCode == 200 {
		clearError()
	} else if err == nil && resp.StatusCode == 404 {
		log.Printf("Callback returned 404, deleting subscription %s", subscriptionID.Hex())
		p.Events.DeleteEvent(subscriptionID)
		clearError()
	} else {
		count := incrementError()

		if count >= 10 {
			log.Printf("Deleting subscription %s due to 10 consecutive failures", subscriptionID.Hex())
			p.Events.DeleteEvent(subscriptionID)
			clearError()
		}
	}
}

// isBlockedCallbackIP reports whether ip is a loopback, private, link-local,
// unspecified, or multicast address - i.e. not something a relay should ever
// make an outbound request to on a subscriber's behalf.
func isBlockedCallbackIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// Middleware

func (p *PushManager) Enable(instance *Instance) {
	p.client = &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			// Guard against SSRF via a push subscription's callback URL,
			// including DNS rebinding (a hostname resolving to a public address
			// at registration time but an internal one at delivery time, or a
			// different one on each lookup): net.Dialer's Control hook fires
			// with the literal address about to be connected to, after DNS
			// resolution and right before the actual socket connect, so this
			// check can't be bypassed by whatever the hostname resolves to.
			DialContext: (&net.Dialer{
				Timeout: 10 * time.Second,
				Control: func(network, address string, c syscall.RawConn) error {
					host, _, err := net.SplitHostPort(address)
					if err != nil {
						return err
					}

					ip := net.ParseIP(host)
					if ip == nil {
						return fmt.Errorf("refusing to dial unparseable address %q", address)
					}

					if isBlockedCallbackIP(ip) {
						return fmt.Errorf("refusing to dial local/internal address %q", address)
					}

					return nil
				},
			}).DialContext,
		},
	}
	p.errorCounts = make(map[string]int)

	instance.Relay.Info.SupportedNIPs = append(instance.Relay.Info.SupportedNIPs, "9a")
}
