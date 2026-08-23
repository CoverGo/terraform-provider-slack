package slack

import (
	"errors"
	"fmt"

	"github.com/slack-go/slack"
)

// Rendering of Slack API errors.
//
// Every call site used to build its summary as "Slack provider couldn't <do X>
// (<subject>) due to *<err>*". That reads as "the object is missing or
// misconfigured" whatever actually went wrong — so a rate limit surfaced as
// "couldn't find a slack user (someone@covergo.com)", and the obvious next step
// was to go and check whether that person still had a Slack account. They did.
// The API was simply refusing the request for volume.
//
// These helpers keep each site's own wording as the fallback, and lead with the
// real cause when it is one we can name.

// slackErrSummary renders the diagnostic summary for a failed API call.
// `context` is the site's own description of what it was doing, e.g.
// "Slack provider couldn't find a slack user (someone@covergo.com)".
func slackErrSummary(err error, context string) string {
	var rateLimited *slack.RateLimitedError
	if errors.As(err, &rateLimited) {
		return fmt.Sprintf("Slack rate limit exceeded — retry after %s. Failed while: %s",
			rateLimited.RetryAfter, context)
	}

	var statusCode slack.StatusCodeError
	if errors.As(err, &statusCode) {
		return fmt.Sprintf("Slack returned HTTP %d (%s). Failed while: %s",
			statusCode.Code, statusCode.Status, context)
	}

	return fmt.Sprintf("%s due to *%s*", context, err.Error())
}

// slackErrDetail renders the diagnostic detail. `method` is the API method's
// documentation URL, which is what the site would otherwise point at.
func slackErrDetail(err error, method string) string {
	var rateLimited *slack.RateLimitedError
	if errors.As(err, &rateLimited) {
		return fmt.Sprintf(
			"This is a rate limit, not a missing or misconfigured object: Slack rejected the "+
				"request for volume and asked us to wait %s. Limits are applied per method per "+
				"workspace — see https://api.slack.com/docs/rate-limits. The provider retries "+
				"rate-limited requests automatically, so seeing this means the retries were also "+
				"exhausted; applying with -parallelism=1, or in smaller changes, will help. "+
				"Method: %s",
			rateLimited.RetryAfter, method)
	}

	return fmt.Sprintf("Please refer to %s for the details.", method)
}
