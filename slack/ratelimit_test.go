package slack

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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

// A persistent 429 must stop after maxRetries rather than retrying forever, and
// the response handed back must still carry a parsable Retry-After: slack-go
// parses that header with strconv and returns the PARSE error when it is
// unusable, which would report a rate limit as an invalid-syntax error.
func Test_rateLimitedClient_givesUpAndKeepsRetryAfterParsable(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		// Deliberately no Retry-After header.
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(ts.Close)

	client := newRateLimitedClient()
	client.maxRetries = 2

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/usergroups.list", strings.NewReader("token=test"))
	if err != nil {
		t.Fatalf("err building request: %s", err)
	}

	// Without a Retry-After the client falls back to defaultWait; shrink it so
	// the test does not sleep out two real cooldowns.
	client.defaultWait = 10 * time.Millisecond

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected the 429 to be handed back, got %d", resp.StatusCode)
	}

	mu.Lock()
	got := calls
	mu.Unlock()
	if want := client.maxRetries + 1; got != want {
		t.Fatalf("expected %d attempts, got %d", want, got)
	}

	if _, err := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 64); err != nil {
		t.Fatalf("Retry-After must stay parsable for slack-go, got %q: %s",
			resp.Header.Get("Retry-After"), err)
	}
}

// A long Retry-After is a one-off cooldown, not the ongoing pace. Carrying it
// forward as the interval would pin the method to one request per Retry-After
// for the rest of the run and turn a large apply into hours.
func Test_rateLimitedClient_sustainedSpacingIsBounded(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "600") // ten minutes
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(ts.Close)

	client := newRateLimitedClient()
	client.maxRetries = 0 // give up immediately; we only want the bookkeeping

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/usergroups.update", strings.NewReader("token=test"))
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
	deadline := client.nextAllowed["/api/usergroups.update"]
	client.mu.Unlock()

	if spacing > maxSustainedSpacing {
		t.Fatalf("sustained spacing %s exceeds the %s cap", spacing, maxSustainedSpacing)
	}
	// The full cooldown is still honoured, just not as the ongoing interval.
	if until := time.Until(deadline); until < time.Minute {
		t.Fatalf("expected the full Retry-After cooldown, deadline is only %s away", until)
	}
}

// A cancelled context must abandon the wait instead of sleeping it out, so
// interrupting Terraform does not hang on a backing-off request.
func Test_rateLimitedClient_honoursContextCancellation(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(ts.Close)

	client := newRateLimitedClient()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/api/usergroups.list", strings.NewReader("token=test"))
	if err != nil {
		t.Fatalf("err building request: %s", err)
	}

	// Cancel while the client is sleeping off the first 429.
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	if _, err := client.Do(req); err == nil {
		t.Fatal("expected an error once the context was cancelled")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("expected the wait to be abandoned promptly, took %s", elapsed)
	}
}

// The ceiling on a single wait is deliberate, and so is the fact that it
// disobeys the server above that point: a malformed or hostile Retry-After must
// not park a CI job, which has nobody to interrupt it.
//
// Both halves matter. Clipping a long value is the behaviour being chosen, and
// NOT clipping a realistic one is what keeps that choice cheap — observed Slack
// values are 30-60s, so nothing real is cut short. A regression in either
// direction is a behaviour change, not a tuning detail.
func Test_rateLimitedClient_capsTheRetryAfterWait(t *testing.T) {
	client := newRateLimitedClient()

	for _, tc := range []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"a realistic cooldown is honoured in full", "60", 60 * time.Second},
		{"the ceiling itself passes through", "120", maxRetryAfter},
		{"anything longer is clipped to the ceiling", "600", maxRetryAfter},
		{"an unusable header falls back", "", defaultRetryAfter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{}}
			if tc.header != "" {
				resp.Header.Set("Retry-After", tc.header)
			}

			if got := client.retryAfterFrom(resp); got != tc.want {
				t.Fatalf("Retry-After %q became %s, want %s", tc.header, got, tc.want)
			}
		})
	}
}

// Backing off must not hold a concurrency slot. With the default cap of two, a
// couple of 429s from one method would otherwise occupy every slot for the
// whole cooldown and stall methods Slack never complained about.
func Test_rateLimitedClient_backoffDoesNotBlockOtherMethods(t *testing.T) {
	limited := make(chan struct{}, 8)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/usergroups.users.update", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)

		// Never block the handler: these requests keep retrying until the test
		// cancels them, and a full channel here would wedge the server's
		// shutdown rather than fail the test.
		select {
		case limited <- struct{}{}:
		default:
		}
	})
	mux.HandleFunc("/api/usergroups.list", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	client := newRateLimitedClient()

	// Fill both slots with requests that are about to back off.
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < cap(client.slots); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost,
				ts.URL+"/api/usergroups.users.update", strings.NewReader("token=test"))
			if err != nil {
				return
			}
			if resp, err := client.Do(req); err == nil {
				_ = resp.Body.Close()
			}
		}()
	}
	defer func() {
		cancel()
		wg.Wait()
	}()

	// Wait until both are in their cooldown rather than on the wire.
	for i := 0; i < cap(client.slots); i++ {
		select {
		case <-limited:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for the limited method to be refused")
		}
	}

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/usergroups.list", strings.NewReader("token=test"))
	if err != nil {
		t.Fatalf("err building request: %s", err)
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("err on the unrelated method: %s", err)
	}
	_ = resp.Body.Close()

	// The backing-off requests are sleeping for three seconds. An unrelated
	// method must not be waiting on them.
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("an unrelated method waited %s on another method's cooldown", elapsed)
	}
}
