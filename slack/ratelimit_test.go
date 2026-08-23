package slack

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// A 429 must be retried, and the retry must carry the same body. Replaying a
// drained body would send an empty form, which Slack answers with
// invalid_arguments — a failure that looks nothing like a rate limit and would
// quietly write the wrong thing.
func Test_rateLimitedClient_retriesAndReplaysBody(t *testing.T) {
	const payload = "token=test&usergroup=S0615G0KT&users=U1,U2"

	var (
		mu     sync.Mutex
		bodies []string
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)

		mu.Lock()
		bodies = append(bodies, string(b))
		attempt := len(bodies)
		mu.Unlock()

		if attempt == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)

	client := newRateLimitedClient()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/usergroups.users.update", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("err building request: %s", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected the retry to succeed, got %d", resp.StatusCode)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(bodies) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(bodies))
	}
	for i, body := range bodies {
		if body != payload {
			t.Fatalf("attempt %d sent %q, want %q", i+1, body, payload)
		}
	}
}

// Once a method has been rate limited, later calls to it are spaced out rather
// than fired straight back into the same limit.
func Test_rateLimitedClient_backsOffPerMethod(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()

		if first {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)

	client := newRateLimitedClient()
	url := ts.URL + "/api/usergroups.update"

	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader("token=test"))
	if err != nil {
		t.Fatalf("err building request: %s", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	_ = resp.Body.Close()

	client.mu.Lock()
	spacing := client.spacing["/api/usergroups.update"]
	untouched := client.spacing["/api/usergroups.list"]
	client.mu.Unlock()

	if spacing <= 0 {
		t.Fatalf("expected the limited method to be spaced, got %s", spacing)
	}
	if untouched != 0 {
		t.Fatalf("expected other methods to stay unthrottled, got %s", untouched)
	}
}
