package alert

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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

// webhookToken is the part of a webhook address that lets anyone who has it
// post to the channel. It sits in the path and in the query, which is where
// the chat services put it.
const webhookToken = "T000-B000-webhook-token" // hook:allow

// refusedAddress is an address on this system that nothing listens on.
func refusedAddress(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	address := listener.Addr().String()
	_ = listener.Close()

	return address
}

// TestAWebhookFailureDoesNotNameTheAddress fails a post every way one fails
// and holds what the failure says, and what Send logs of it, to carry no part
// of the path or the query, which is where the token of a webhook is.
func TestAWebhookFailureDoesNotNameTheAddress(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()

	var looping *httptest.Server
	looping = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, looping.URL+"/moved/"+webhookToken+"?token="+webhookToken, http.StatusTemporaryRedirect)
	}))
	defer looping.Close()

	secret := "/services/" + webhookToken + "?token=" + webhookToken

	cases := []struct {
		name string
		url  string
		want string
	}{
		{"refused", "http://" + refusedAddress(t) + secret, "Post to the webhook: "},
		{"500", failing.URL + secret, "500"},
		{"redirect loop", looping.URL + secret, "redirects"},
		{"unreadable", "http://127.0.0.1/" + webhookToken + "\x7f", "parse to the webhook: "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			sender := NewSender(zap.New(core), nil)

			err := sender.Webhook(context.Background(), tc.url, Event{Event: EventTest})
			if err == nil {
				t.Fatal("the post was reported as delivered")
			}

			if strings.Contains(err.Error(), webhookToken) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the failure says %q, want one with %q and without the token", err, tc.want)
			}

			sender.Send(context.Background(), Config{WebhookURL: tc.url}, Event{Event: EventTest})

			entries := logs.All()
			if len(entries) == 0 {
				t.Fatal("Send logged nothing of the failure")
			}

			for _, entry := range entries {
				line := entry.Message
				for key, value := range entry.ContextMap() {
					line += fmt.Sprintf(" %s=%v", key, value)
				}

				if strings.Contains(line, webhookToken) {
					t.Errorf("Send logged the token: %s", line)
				}
			}
		})
	}
}
