package alert

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestTheWebhookIsPostedTheEventAsJSON reads what arrives at the webhook and
// holds every field to the one the event carried.
func TestTheWebhookIsPostedTheEventAsJSON(t *testing.T) {
	type received struct {
		method      string
		contentType string
		body        map[string]interface{}
	}

	got := make(chan received, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)

		var body map[string]interface{}
		_ = json.Unmarshal(raw, &body)

		got <- received{method: r.Method, contentType: r.Header.Get("Content-Type"), body: body}

		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	sender := NewSender(zap.NewNop(), nil)

	event := Event{
		Event:        EventDown,
		Kind:         KindLocalForward,
		Host:         "ssh.example.com",
		LocalPort:    15432,
		Since:        "2026-09-26T01:00:00Z",
		LastError:    "ssh: handshake failed",
		Installation: "tm.example.com",
	}

	err := sender.Webhook(context.Background(), server.URL+"/hook", event)
	if err != nil {
		t.Fatalf("Webhook: %v", err)
	}

	r := <-got

	if r.method != http.MethodPost || r.contentType != "application/json" {
		t.Fatalf("the webhook was sent %s with %q, want POST with application/json", r.method, r.contentType)
	}

	want := map[string]interface{}{
		"event":        "down",
		"kind":         "local_forward",
		"host":         "ssh.example.com",
		"local_port":   float64(15432),
		"since":        "2026-09-26T01:00:00Z",
		"last_error":   "ssh: handshake failed",
		"installation": "tm.example.com",
	}

	if len(r.body) != len(want) {
		t.Fatalf("the body carries %v, want exactly %v", r.body, want)
	}

	for key, value := range want {
		if r.body[key] != value {
			t.Errorf("%s = %v, want %v", key, r.body[key], value)
		}
	}
}

// TestAWebhookThatRefusesIsAFailure is an answer outside 2xx.
func TestAWebhookThatRefusesIsAFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	err := NewSender(zap.NewNop(), nil).Webhook(context.Background(), server.URL, Event{Event: EventTest})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a webhook that answered 403 gave %v, want a failure naming the status", err)
	}
}

// TestTheWebhookIsGivenUpOnAfterTheTimeout holds a webhook that does not answer
// to the timeout. The ten seconds are held as the setting, and a shorter one is
// used to see that it is what ends the post.
func TestTheWebhookIsGivenUpOnAfterTheTimeout(t *testing.T) {
	sender := NewSender(zap.NewNop(), nil)

	if sender.client.Timeout != 10*time.Second {
		t.Fatalf("the webhook timeout is %v, want 10s", sender.client.Timeout)
	}

	release := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer server.Close()
	defer close(release)

	sender.client.Timeout = 200 * time.Millisecond

	started := time.Now()

	err := sender.Webhook(context.Background(), server.URL, Event{Event: EventTest})
	if err == nil {
		t.Fatal("a webhook that never answered was reported as posted")
	}

	if waited := time.Since(started); waited > 5*time.Second {
		t.Fatalf("the post was given up on after %v, want about the timeout", waited)
	}
}

// TestAWebhookRedirectIsFollowedThreeTimesAtMost is a webhook that redirects to
// itself for ever.
func TestAWebhookRedirectIsFollowedThreeTimesAtMost(t *testing.T) {
	hits := 0

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, server.URL+"/again", http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	err := NewSender(zap.NewNop(), nil).Webhook(context.Background(), server.URL, Event{Event: EventTest})
	if err == nil || !strings.Contains(err.Error(), "redirects") {
		t.Fatalf("a redirect loop gave %v, want a failure about redirects", err)
	}

	// The first post and the three redirects that are followed.
	if hits != maxWebhookRedirects+1 {
		t.Fatalf("the webhook was asked %d times, want %d", hits, maxWebhookRedirects+1)
	}
}
