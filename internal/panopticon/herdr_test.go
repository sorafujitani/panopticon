package panopticon

import "testing"

func TestHerdrPayloadExtractionIgnoresCLIWrapperID(t *testing.T) {
	payload := map[string]any{
		"id": "cli:request-1",
		"result": map[string]any{
			"workspace": map[string]any{"id": "w-workspace"},
			"tabId":     "w-tab",
			"root_pane": map[string]any{"pane_id": "w-pane"},
			"worktree":  map[string]any{"checkout_path": "/tmp/worktree"},
			"agent":     map[string]any{"state": "BLOCKED"},
			"error":     map[string]any{"code": "agent blocked"},
		},
	}
	workspace, err := ExtractID(payload, "workspace")
	if err != nil || workspace != "w-workspace" {
		t.Fatalf("workspace id: %q, %v", workspace, err)
	}
	tab, err := ExtractID(payload, "tab")
	if err != nil || tab != "w-tab" {
		t.Fatalf("tab id: %q, %v", tab, err)
	}
	pane, err := ExtractID(payload, "pane")
	if err != nil || pane != "w-pane" {
		t.Fatalf("pane id: %q, %v", pane, err)
	}
	if ExtractPath(payload) != "/tmp/worktree" || ExtractStatus(payload) != "blocked" || ExtractErrorCode(payload) != "agent_blocked" {
		t.Fatalf("nested payload extraction failed")
	}
}

func TestParseJSONOutputUsesLastNoisyDocument(t *testing.T) {
	payload := ParseJSONOutput("log before\n{\"status\":\"old\"}\nlog after\n{\"status\":\"new\"}\n")
	if mapValue(payload)["status"] != "new" {
		t.Fatalf("unexpected payload: %#v", payload)
	}
}

func TestExtractsPathStatusAndErrorCodeFromNestedPayload(t *testing.T) {
	getPayload := map[string]any{
		"status": "ok",
		"result": map[string]any{
			"agent": map[string]any{"agent_status": " WORKING "},
			"type":  "agent_info",
		},
	}
	if ExtractStatus(getPayload) != "working" {
		t.Fatalf("status=%q", ExtractStatus(getPayload))
	}
}
