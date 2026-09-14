package slack

import (
	"context"
	"errors"
	"sync"

	"github.com/slack-go/slack"
)

// One usergroups.list fetch at a time, and everyone waiting shares its outcome.
//
// The cache check and the fetch are separate steps, so every Terraform worker
// that misses would otherwise issue its own request before the first one wrote
// the file. The slot cap in the rate limited client bounds how many of those
// fly at once, not how many are made — with the default -parallelism of 10, one
// expiry of the six second cache can cost ten calls instead of one.
//
// Sharing the outcome rather than just the slot matters on the failing path.
// Serialising the fetch alone would leave each queued caller to find the cache
// still empty and try again in turn, so N workers cost N sequential failures —
// and when the failure is an exhausted rate limit, each of those is itself
// several cooldowns long.
type userGroupFetch struct {
	done   chan struct{}
	groups []slack.UserGroup
	err    error
}

var (
	userGroupFetchMu sync.Mutex
	userGroupFetchIn *userGroupFetch
)

// One cached usergroups.list for everything that needs to read a usergroup.
//
// usergroups.list returns every group in a single response, and with
// include_users it carries each group's membership too. Three reads need that
// data — the usergroup itself, its default channels, and its members — and
// before this they were split: the first two shared a cached list, while
// members called usergroups.users.list once per group. A refresh of 80-odd
// teams therefore issued 80-odd extra requests against a Tier 2 endpoint
// (~20/min), which is enough on its own to rate limit a plan.
//
// Fetching with IncludeUsers means one call serves all three. The flag has to
// be set by every caller that populates the cache, or a reader that needs
// members can find a cached list that has none.
func cachedUserGroups(ctx context.Context, client *slack.Client) ([]slack.UserGroup, error) {
	for {
		if groups, ok := restoreUserGroupCache(); ok {
			return groups, nil
		}

		fetch, leader := joinUserGroupFetch()
		if leader {
			fetch.groups, fetch.err = fetchUserGroups(ctx, client)
			finishUserGroupFetch(fetch)

			return fetch.groups, fetch.err
		}

		select {
		case <-fetch.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}

		if fetch.err == nil {
			return fetch.groups, nil
		}

		// The fetch this caller waited on failed. If it failed because the
		// leader's own context went away while this one is still good — a
		// different resource being cancelled, say — inheriting that would
		// report a cancellation that was never ours. Go round again instead.
		cancelled := errors.Is(fetch.err, context.Canceled) || errors.Is(fetch.err, context.DeadlineExceeded)
		if cancelled && ctx.Err() == nil {
			continue
		}

		return nil, fetch.err
	}
}

// joinUserGroupFetch either claims the next fetch or returns the one already in
// flight to wait on.
func joinUserGroupFetch() (*userGroupFetch, bool) {
	userGroupFetchMu.Lock()
	defer userGroupFetchMu.Unlock()

	if userGroupFetchIn != nil {
		return userGroupFetchIn, false
	}

	userGroupFetchIn = &userGroupFetch{done: make(chan struct{})}

	return userGroupFetchIn, true
}

// finishUserGroupFetch publishes the result to everyone waiting on it. Clearing
// the in-flight pointer first means a caller arriving now starts a fresh fetch
// rather than joining one whose result is already settled.
func finishUserGroupFetch(fetch *userGroupFetch) {
	userGroupFetchMu.Lock()
	userGroupFetchIn = nil
	userGroupFetchMu.Unlock()

	close(fetch.done)
}

func fetchUserGroups(ctx context.Context, client *slack.Client) ([]slack.UserGroup, error) {
	// Look again before going to Slack: the fetch this caller is leading was
	// claimed after its cache check, and whoever led the previous one may have
	// filled the cache in between.
	if groups, ok := restoreUserGroupCache(); ok {
		return groups, nil
	}

	userGroups, err := client.GetUserGroupsContext(ctx, func(params *slack.GetUserGroupsParams) {
		// Carries each group's members, so usergroups.users.list is not needed.
		params.IncludeUsers = true
		params.IncludeCount = false
		params.IncludeDisabled = true
	})
	if err != nil {
		return nil, err
	}

	saveCacheAsJson(userGroupListCacheFileName, &userGroups)

	return userGroups, nil
}

func restoreUserGroupCache() ([]slack.UserGroup, bool) {
	var cached *[]slack.UserGroup

	if restoreJsonCache(userGroupListCacheFileName, &cached) && cached != nil {
		return *cached, true
	}

	return nil, false
}
