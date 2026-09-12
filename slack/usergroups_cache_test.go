package slack

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/slack-go/slack"
)

// cacheTestServer returns a client pointed at a fake Slack that records every
// usergroups.list request, along with a counter and the include_users values it
// was called with.
func cacheTestServer(t *testing.T, groups []slack.UserGroup) (*slack.Client, func() (int, []string)) {
	t.Helper()

	var (
		mu           sync.Mutex
		calls        int
		includeUsers []string
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/usergroups.list", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()

		mu.Lock()
		calls++
		includeUsers = append(includeUsers, r.Form.Get("include_users"))
		mu.Unlock()

		renderJson(w, userGroupListResponse{slack.SlackResponse{Ok: true}, groups})
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	client := slack.New("test_token",
		slack.Option(slack.OptionHTTPClient(ts.Client())),
		slack.OptionAPIURL(ts.URL+"/"))

	return client, func() (int, []string) {
		mu.Lock()
		defer mu.Unlock()
		return calls, append([]string(nil), includeUsers...)
	}
}

// The cached list has to be fetched WITH members. Every caller shares one cache
// file, so a fetch without include_users would leave a reader that needs
// membership looking at a list that has none — and it would then set the group's
// members to empty, silently emptying it on the next apply.
func Test_cachedUserGroups_requestsUsersAndServesFromCache(t *testing.T) {
	clearUserGroupCache(t)

	client, stats := cacheTestServer(t, []slack.UserGroup{testUserGroup})

	first, err := cachedUserGroups(context.Background(), client)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if len(first) != 1 || first[0].ID != testUserGroup.ID {
		t.Fatalf("unexpected groups: %+v", first)
	}
	if len(first[0].Users) != len(testUserGroup.Users) {
		t.Fatalf("expected the cached list to carry members, got %v", first[0].Users)
	}

	calls, includeUsers := stats()
	if calls != 1 {
		t.Fatalf("expected 1 API call, got %d", calls)
	}
	if includeUsers[0] != "true" {
		t.Fatalf("expected include_users=true, got %q", includeUsers[0])
	}

	// A second read inside the cache window must not hit the API again: that is
	// the entire point, ~83 per-group calls collapsing to one.
	second, err := cachedUserGroups(context.Background(), client)
	if err != nil {
		t.Fatalf("err on the cached read: %s", err)
	}
	if len(second) != 1 || second[0].ID != testUserGroup.ID {
		t.Fatalf("cached read returned something else: %+v", second)
	}
	if len(second[0].Users) != len(testUserGroup.Users) {
		t.Fatalf("cached read lost the members: %v", second[0].Users)
	}

	if calls, _ := stats(); calls != 1 {
		t.Fatalf("expected the second read to be served from cache, saw %d API calls", calls)
	}
}

// An unreadable cache must fall back to the API rather than returning nothing.
// Returning an empty list here would look like "no groups exist" and empty every
// managed group.
func Test_cachedUserGroups_refetchesWhenCacheUnreadable(t *testing.T) {
	clearUserGroupCache(t)

	client, stats := cacheTestServer(t, []slack.UserGroup{testUserGroup})

	if _, err := cachedUserGroups(context.Background(), client); err != nil {
		t.Fatalf("err: %s", err)
	}

	if err := os.WriteFile(filepath.Join(cacheDir, userGroupListCacheFileName), []byte("{ not json"), 0644); err != nil {
		t.Fatalf("err corrupting the cache: %s", err)
	}

	groups, err := cachedUserGroups(context.Background(), client)
	if err != nil {
		t.Fatalf("err after corrupting the cache: %s", err)
	}
	if len(groups) != 1 || groups[0].ID != testUserGroup.ID {
		t.Fatalf("expected a refetch to recover the list, got %+v", groups)
	}
	if calls, _ := stats(); calls != 2 {
		t.Fatalf("expected a second API call after the cache went bad, saw %d", calls)
	}
}

// A group that has disappeared from Slack must clear the resource id so
// Terraform plans to recreate it, rather than reporting empty membership — which
// would read as "everyone left this group" and plan to remove them all.
func Test_ResourceUserGroupMembersRead_missingGroupClearsId(t *testing.T) {
	clearUserGroupCache(t)

	d := resourceSlackUserGroupMembers().TestResourceData()
	d.SetId(testUserGroup.ID)
	if err := d.Set("usergroup_id", testUserGroup.ID); err != nil {
		t.Fatalf("err: %s", err)
	}

	ctx, team := createTestTeam(t, Routes{
		{
			Path: "/usergroups.list",
			Response: userGroupListResponse{
				slack.SlackResponse{Ok: true},
				[]slack.UserGroup{{ID: "SOMEONEELSE"}},
			},
		},
	})

	if diags := resourceSlackUserGroupMembersRead(ctx, d, team); diags.HasError() {
		t.Fatalf("expected no error for a missing group, got %v", diags)
	}

	if d.Id() != "" {
		t.Fatalf("expected the id to be cleared, got %q", d.Id())
	}
	if members := d.Get("members").(*schema.Set); members.Len() != 0 {
		t.Fatalf("expected no members to be recorded, got %d", members.Len())
	}
}

// Concurrent misses must collapse into a single usergroups.list.
//
// The cache check and the fetch are separate steps, so without the fetch slot
// every Terraform worker that misses issues its own request before the first
// writes the file — the burst this cache exists to remove.
func Test_cachedUserGroups_collapsesConcurrentMisses(t *testing.T) {
	clearUserGroupCache(t)

	var (
		mu    sync.Mutex
		calls int
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/usergroups.list", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()

		// Wide enough for every caller to have missed the cache already.
		time.Sleep(50 * time.Millisecond)

		renderJson(w, userGroupListResponse{slack.SlackResponse{Ok: true}, []slack.UserGroup{testUserGroup}})
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	client := slack.New("test_token",
		slack.Option(slack.OptionHTTPClient(ts.Client())),
		slack.OptionAPIURL(ts.URL+"/"))

	const workers = 8

	var (
		wg      sync.WaitGroup
		results = make([][]slack.UserGroup, workers)
		errs    = make([]error, workers)
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = cachedUserGroups(context.Background(), client)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %s", i, err)
		}
		if len(results[i]) != 1 || results[i][0].ID != testUserGroup.ID {
			t.Fatalf("worker %d got %+v", i, results[i])
		}
		if len(results[i][0].Users) != len(testUserGroup.Users) {
			t.Fatalf("worker %d lost the members: %v", i, results[i][0].Users)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("expected %d concurrent readers to share 1 API call, saw %d", workers, calls)
	}
}

// A failing fetch is shared too, not repeated once per waiter.
//
// Sharing only the fetch slot would serialise the failure instead of removing
// it: each queued caller finds the cache still empty and tries again in turn, so
// the cost grows with the worker count. When the failure is an exhausted rate
// limit — the case this whole change exists for — each of those attempts is
// itself several cooldowns long.
func Test_cachedUserGroups_sharesTheFailure(t *testing.T) {
	clearUserGroupCache(t)

	var (
		mu    sync.Mutex
		calls int
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/usergroups.list", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()

		// Wide enough for every caller to have queued behind this one.
		time.Sleep(50 * time.Millisecond)

		w.WriteHeader(http.StatusInternalServerError)
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	client := slack.New("test_token",
		slack.Option(slack.OptionHTTPClient(ts.Client())),
		slack.OptionAPIURL(ts.URL+"/"))

	const workers = 8

	var (
		wg   sync.WaitGroup
		errs = make([]error, workers)
	)

	start := time.Now()
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = cachedUserGroups(context.Background(), client)
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	for i, err := range errs {
		if err == nil {
			t.Fatalf("worker %d saw no error from a failing API", i)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("expected %d workers to share 1 failed call, saw %d", workers, calls)
	}

	// Serialised retries would cost roughly one 50ms failure per worker.
	if elapsed > 200*time.Millisecond {
		t.Fatalf("expected the failure to be shared, but %d workers took %s", workers, elapsed.Round(time.Millisecond))
	}
}
