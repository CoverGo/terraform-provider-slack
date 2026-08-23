package slack

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"
)

// Rate limit handling for every call the provider makes.
//
// Slack limits per method per workspace, and the tiers that matter here are
// strict: usergroups.update and usergroups.users.update are Tier 2, around 20
// requests a minute. A Terraform apply that touches every usergroup issues far
// more than that in a burst — the provider used to fire them 10-wide and get a
// second in before Slack started refusing, which left applies half-finished.
//
// slack-go surfaces a 429 as *slack.RateLimitedError carrying the server's
// Retry-After, and then simply returns it. Nothing retried. This client sits
// underneath slack-go (via slack.OptionHTTPClient) so every method is covered
// at once, including any added later.
//
// Two behaviours:
//
//   - Retry a 429 for as long as Slack asks, up to maxRetries.
//   - After a method is rate limited once, keep a minimum spacing between
//     subsequent calls to THAT method for the rest of the run. Backing off only
//     the method that complained keeps unrelated calls at full speed, and stops
//     the next burst from walking straight back into the same wall.
//
// Nothing is throttled until Slack objects, so a small change stays fast.

const (
	defaultMaxRetries = 5

	// Fallback when a 429 arrives without a usable Retry-After.
	defaultRetryAfter = 30 * time.Second

	// Ceiling on a single sleep, so a hostile value cannot hang an apply.
	maxRetryAfter = 2 * time.Minute
)

// rateLimitedClient is an http.Client wrapper satisfying slack-go's httpClient
// interface (a single Do method).
type rateLimitedClient struct {
	inner      *http.Client
	maxRetries int

	mu sync.Mutex
	// Earliest time the next request to a given API method may start. Only
	// populated for methods that have actually been rate limited.
	nextAllowed map[string]time.Time
	// Spacing to keep for a method once it has pushed back.
	spacing map[string]time.Duration
}

func newRateLimitedClient() *rateLimitedClient {
	retries := defaultMaxRetries
	if v := os.Getenv("SLACK_MAX_RETRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			retries = n
		}
	}

	return &rateLimitedClient{
		inner:       &http.Client{Timeout: 60 * time.Second},
		maxRetries:  retries,
		nextAllowed: map[string]time.Time{},
		spacing:     map[string]time.Duration{},
	}
}

// method identifies the Slack API method from the request path, e.g.
// "/api/usergroups.update". Limits are per method, so that is the bucket key.
func method(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Path
}

// reserve blocks until this method's next slot, then claims the one after it.
func (c *rateLimitedClient) reserve(req *http.Request) error {
	key := method(req.URL)

	c.mu.Lock()
	gap, throttled := c.spacing[key]
	if !throttled {
		c.mu.Unlock()
		return nil // this method has never been limited; go at full speed
	}
	start := time.Now()
	if t, ok := c.nextAllowed[key]; ok && t.After(start) {
		start = t
	}
	c.nextAllowed[key] = start.Add(gap)
	c.mu.Unlock()

	return sleepUntil(req, time.Until(start))
}

// penalise records that a method pushed back, so later calls to it are spaced.
func (c *rateLimitedClient) penalise(req *http.Request, retryAfter time.Duration) {
	key := method(req.URL)

	c.mu.Lock()
	defer c.mu.Unlock()
	if retryAfter > c.spacing[key] {
		c.spacing[key] = retryAfter
	}
	// Hold every other in-flight call to this method until the wait is over,
	// rather than letting them queue up behind their own 429s.
	if t := time.Now().Add(retryAfter); t.After(c.nextAllowed[key]) {
		c.nextAllowed[key] = t
	}
}

func sleepUntil(req *http.Request, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-req.Context().Done():
		return req.Context().Err()
	}
}

func retryAfterFrom(resp *http.Response) time.Duration {
	if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 {
		d := time.Duration(secs) * time.Second
		if d > maxRetryAfter {
			return maxRetryAfter
		}
		return d
	}
	return defaultRetryAfter
}

func (c *rateLimitedClient) Do(req *http.Request) (*http.Response, error) {
	// The body has to be replayable: these are POST forms, and a retry that
	// reuses a drained body sends an empty form, which Slack answers with a
	// confusing invalid_arguments rather than anything about rate limits.
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
	}

	for attempt := 0; ; attempt++ {
		if err := c.reserve(req); err != nil {
			return nil, err
		}
		if body != nil {
			req.Body = io.NopCloser(bytes.NewReader(body))
		}

		resp, err := c.inner.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			return resp, nil
		}

		wait := retryAfterFrom(resp)
		c.penalise(req, wait)

		// Out of attempts: hand the 429 back so slack-go turns it into a
		// *slack.RateLimitedError and the diagnostic names the real cause.
		if attempt >= c.maxRetries {
			return resp, nil
		}

		// Drain before discarding so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()

		if err := sleepUntil(req, wait); err != nil {
			return nil, err
		}
	}
}
