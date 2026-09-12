package slack

import (
	"context"

	"github.com/slack-go/slack"
)

// Only one usergroups.list fetch runs at a time.
//
// The cache check and the fetch are not one step: every Terraform worker that
// misses the cache would otherwise issue its own request before the first one
// wrote the file. The slot cap in the rate limited client bounds how many of
// those fly at once, not how many are made — with the default -parallelism of
// 10, one expiry of the six second cache can cost ten calls instead of one.
//
// Callers that arrive while a fetch is in flight wait for it and then find the
// cache populated, so the burst collapses back to the single request this is
// supposed to be.
var userGroupFetch = make(chan struct{}, 1)

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
	if groups, ok := restoreUserGroupCache(); ok {
		return groups, nil
	}

	select {
	case userGroupFetch <- struct{}{}:
		defer func() { <-userGroupFetch }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// Look again now the fetch slot is held: whoever held it before may have
	// filled the cache while this call was waiting, in which case there is
	// nothing left to ask Slack for.
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
