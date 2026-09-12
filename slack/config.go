package slack

import (
	"github.com/slack-go/slack"
)

type Config struct {
	Token string
}

type Team struct {
	client *slack.Client
	logger *Logger
}

func (c *Config) ProviderContext(version string, commit string) (*Team, error) {
	var team Team

	// Every API call goes through the rate-limit aware client (ratelimit.go):
	// it retries a 429 for as long as Slack asks, and afterwards spaces out
	// further calls to that one method. Injecting it here covers every method
	// the provider uses, and any added later, without touching resource code.
	team.client = slack.New(c.Token, slack.OptionHTTPClient(newRateLimitedClient()))
	team.logger = configureLogger(version, commit)

	return &team, nil
}
