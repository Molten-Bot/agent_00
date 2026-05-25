package hub

import "testing"

func TestPublishResultA2ARoutingMetadata(t *testing.T) {
	t.Parallel()

	payload := map[string]any{
		"status":       "completed",
		"to_agent_uri": "https://na.hub.molten.bot/agents/target",
	}
	got := publishResultA2ARoutingMetadata(payload)
	if got["to_agent_uri"] != "https://na.hub.molten.bot/agents/target" {
		t.Fatalf("publishResultA2ARoutingMetadata() = %#v, want to_agent_uri", got)
	}

	if got := publishResultA2ARoutingMetadata(map[string]any{"status": "ok"}); got != nil {
		t.Fatalf("publishResultA2ARoutingMetadata(unrouted) = %#v, want nil", got)
	}
}

func TestPublishResultRuntimeBodyCarriesOpenAPIFields(t *testing.T) {
	t.Parallel()

	payload := map[string]any{
		"status":      "error",
		"request_id":  "req-1",
		"reply_to":    "agent-123",
		"session_key": "local-session",
	}
	body, routed := publishResultRuntimeBody(payload)
	if !routed {
		t.Fatal("publishResultRuntimeBody() routed = false, want true")
	}
	if got := body["session_key"]; got != "local-session" {
		t.Fatalf("session_key = %#v, want local-session", got)
	}
	if got := body["target"]; got != "agent-123" {
		t.Fatalf("target = %#v, want agent-123", got)
	}
	if got := body["target_agent"]; got != "agent-123" {
		t.Fatalf("target_agent = %#v, want agent-123", got)
	}
	if got := body["to_agent_id"]; got != "agent-123" {
		t.Fatalf("to_agent_id = %#v, want agent-123", got)
	}
	if got := body["client_msg_id"]; got != "req-1" {
		t.Fatalf("client_msg_id = %#v, want req-1", got)
	}
	msg, ok := body["message"].(map[string]any)
	if !ok {
		t.Fatalf("message = %#v, want map payload", body["message"])
	}
	if got := msg["request_id"]; got != "req-1" {
		t.Fatalf("message.request_id = %#v, want req-1", got)
	}
}

func TestPublishResultRuntimeBodySessionOnlyNotA2ARouted(t *testing.T) {
	t.Parallel()

	payload := map[string]any{
		"status":      "error",
		"request_id":  "req-local",
		"session_key": "main",
	}
	body, routed := publishResultRuntimeBody(payload)
	if routed {
		t.Fatal("publishResultRuntimeBody(session-only) routed = true, want false")
	}
	if got := body["session_key"]; got != "main" {
		t.Fatalf("session_key = %#v, want main", got)
	}
	if got := publishResultA2ARoutingMetadata(payload); got != nil {
		t.Fatalf("publishResultA2ARoutingMetadata(session-only) = %#v, want nil", got)
	}
}
