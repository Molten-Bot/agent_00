package hub

import (
	"errors"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/app"
)

func TestHubDispatchDoesNotQueueRepairForRejectedCodexLogin(t *testing.T) {
	t.Parallel()
	for _, err := range []error{
		agentruntime.ErrCodexAuthRequired,
		errors.New("codex: run codex [exec --sandbox workspace-write]: exit status 1 (ERROR: Your access token could not be refreshed because your refresh token was already used. Please log out and sign in again.)"),
		errors.New("refresh_token_reused"),
	} {
		queued, reason := shouldQueueFailureFollowUp(SkillDispatch{RequestID: "auth-failed-task"}, app.Result{ExitCode: app.ExitCodex, Err: err})
		if queued || reason == "" {
			t.Errorf("rejected login queued a Hub repair: queued=%t, reason=%q, error=%v", queued, reason, err)
		}
	}
}
