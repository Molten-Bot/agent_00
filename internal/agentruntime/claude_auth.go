package agentruntime

import (
	"errors"
	"strings"
)

var ErrClaudeAuthRequired = errors.New("Claude authentication required; sign in again, then rerun the failed task")

// ClaudeAuthenticationFailure recognizes provider diagnostics, rather than
// arbitrary HTTP 401s from tools used by the agent.
func ClaudeAuthenticationFailure(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		line = strings.ToLower(strings.TrimSpace(line))
		if strings.HasPrefix(line, "failed to authenticate.") ||
			strings.HasPrefix(line, "failed to authenticate:") ||
			strings.HasPrefix(line, "api error: 401") ||
			strings.HasPrefix(line, "oauth access token has expired") ||
			strings.HasPrefix(line, "invalid api key · please run /login") ||
			// The CLI reports a logged-out session as its final stdout line
			// ("Not logged in · Please run /login"). Without these markers the
			// harness reported a generic agent failure and never asked the
			// operator to sign in again, matching CodexAuthenticationFailure.
			strings.HasPrefix(line, "not logged in") ||
			strings.HasPrefix(line, "please run /login") {
			return true
		}
	}
	return false
}
