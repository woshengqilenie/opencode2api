package protocol

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// codexMixedTools mirrors the tool list a real Codex CLI 0.162 session sends:
// plain functions, a namespace bundling nested functions, a custom free-form
// tool, and a provider-side web_search tool. The old decoder rejected this
// shape with "tools[7] must be a function tool" and broke every Codex session.
func codexMixedTools() []any {
	return []any{
		map[string]any{"type": "function", "name": "exec_command", "description": "Run a command",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"cmd": map[string]any{"type": "string"}}}},
		map[string]any{"type": "namespace", "name": "multi_agent_v1", "description": "Sub-agent tools",
			"tools": []any{
				map[string]any{"type": "function", "name": "close_agent", "description": "Close an agent",
					"parameters": map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}}},
				map[string]any{"type": "function", "name": "spawn_agent", "description": "Spawn an agent",
					"parameters": map[string]any{"type": "object", "properties": map[string]any{"task": map[string]any{"type": "string"}}}},
			}},
		map[string]any{"type": "custom", "name": "apply_patch", "description": "Apply a patch",
			"format": map[string]any{"type": "grammar", "syntax": "lark"}},
		map[string]any{"type": "web_search", "external_web_access": false},
	}
}

func codexPayload() map[string]any {
	return map[string]any{
		"model":  "deepseek-v4.1-flash-free",
		"stream": true,
		"tools":  codexMixedTools(),
		"input": []any{
			map[string]any{"type": "message", "role": "user",
				"content": []any{map[string]any{"type": "input_text", "text": "hi"}}},
		},
	}
}

func TestResponsesToolsToChatAcceptsCodexToolShapes(t *testing.T) {
	tools, aliases := ResponsesToolsToChat(codexPayload())
	byName := map[string]bridgeTool{}
	for _, tool := range tools {
		byName[tool.Name] = tool
	}
	if _, ok := byName["exec_command"]; !ok {
		t.Fatalf("plain function tool lost: %v", byName)
	}
	if _, ok := byName["multi_agent_v1__close_agent"]; !ok {
		t.Fatalf("namespaced tool was not flattened: %v", byName)
	}
	if _, ok := byName["multi_agent_v1__spawn_agent"]; !ok {
		t.Fatalf("namespaced tool was not flattened: %v", byName)
	}
	custom, ok := byName["apply_patch"]
	if !ok {
		t.Fatalf("custom tool missing: %v", byName)
	}
	schema, _ := custom.Schema.(map[string]any)
	properties, _ := schema["properties"].(map[string]any)
	if _, ok := properties["input"]; !ok {
		t.Fatalf("custom tool must accept a single string input: %v", schema)
	}
	if !strings.Contains(custom.Description, "apply_patch") {
		t.Fatalf("custom tool definition was not preserved in the description: %q", custom.Description)
	}
	for name := range byName {
		if name == "" || name == "web_search" {
			t.Fatalf("provider-side tools must be dropped, got %q", name)
		}
	}

	closeAgent, ok := aliases.Lookup("multi_agent_v1__close_agent")
	if !ok || closeAgent.Kind != ToolAliasNamespace || closeAgent.Name != "close_agent" || closeAgent.Namespace != "multi_agent_v1" {
		t.Fatalf("namespace alias wrong: %+v ok=%v", closeAgent, ok)
	}
	customAlias, ok := aliases.Lookup("apply_patch")
	if !ok || customAlias.Kind != ToolAliasCustom {
		t.Fatalf("custom alias wrong: %+v ok=%v", customAlias, ok)
	}
}

// The regression that started this: a Codex request must prepare cleanly.
func TestPrepareRequestAcceptsCodexPayload(t *testing.T) {
	prepared, err := PrepareRequest(Responses, Chat, codexPayload(), "https://upstream.example")
	if err != nil {
		t.Fatalf("preparing a Codex-shaped request must not fail: %v", err)
	}
	encoded, err := json.Marshal(prepared)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(encoded)
	for _, want := range []string{"multi_agent_v1__close_agent", "apply_patch", "exec_command"} {
		if !strings.Contains(body, want) {
			t.Fatalf("upstream body missing %q: %s", want, body[:400])
		}
	}
	if strings.Contains(body, "web_search") {
		t.Fatalf("provider-side tools must not reach the upstream: %s", body[:400])
	}
}

const chatToolsSSE = "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_ns\",\"function\":{\"name\":\"multi_agent_v1__close_agent\",\"arguments\":\"{\\\"id\\\":\\\"a1\\\"}\"}}]}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"call_ct\",\"function\":{\"name\":\"apply_patch\",\"arguments\":\"\"}}]}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":1,\"function\":{\"arguments\":\"{\\\"input\\\":\\\"*** Begin Patch\\\\n*** End Patch\\\"}\"}}]}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
	"data: [DONE]\n\n"

func TestTranscodeCodexToolCallsBackToResponsesItems(t *testing.T) {
	aliases := BuildResponsesAliases(codexPayload())
	recorder := httptest.NewRecorder()
	if _, _, err := TranscodeStreamWithAliases(context.Background(), recorder, strings.NewReader(chatToolsSSE), Chat, Responses, "m", aliases); err != nil {
		t.Fatalf("transcode: %v", err)
	}
	body := recorder.Body.String()
	// Namespaced call: original child name plus the namespace field.
	if !strings.Contains(body, "\"name\":\"close_agent\"") || !strings.Contains(body, "\"namespace\":\"multi_agent_v1\"") {
		t.Fatalf("namespaced call was not restored:\n%s", body)
	}
	// Custom call: custom_tool_call item with the raw free-form input.
	if !strings.Contains(body, "\"type\":\"custom_tool_call\"") || !strings.Contains(body, "response.custom_tool_call_input.done") {
		t.Fatalf("custom call was not restored as a custom_tool_call:\n%s", body)
	}
	if !strings.Contains(body, "*** Begin Patch") {
		t.Fatalf("custom tool input was lost:\n%s", body)
	}
	if strings.Contains(body, "multi_agent_v1__close_agent") {
		t.Fatalf("the flattened chat name leaked to the client:\n%s", body)
	}
	if !strings.Contains(body, "response.completed") {
		t.Fatalf("stream did not complete:\n%s", body)
	}
}

func TestNonStreamCodexToolCallsBackToResponsesItems(t *testing.T) {
	aliases := BuildResponsesAliases(codexPayload())
	chat := []byte(`{"id":"c1","model":"m","created":1,"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[` +
		`{"id":"call_ns","type":"function","function":{"name":"multi_agent_v1__spawn_agent","arguments":"{\"task\":\"x\"}"}},` +
		`{"id":"call_ct","type":"function","function":{"name":"apply_patch","arguments":"{\"input\":\"patch-body\"}"}}]}}]}`)
	converted, err := ConvertResponseWithAliases(Chat, Responses, chat, aliases)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	body := string(converted)
	for _, want := range []string{"\"namespace\":\"multi_agent_v1\"", "\"name\":\"spawn_agent\"", "\"type\":\"custom_tool_call\"", "patch-body"} {
		if !strings.Contains(body, want) {
			t.Fatalf("converted document missing %q:\n%s", want, body)
		}
	}
}

// A Codex replay includes the calls it made earlier; those items must decode
// back into the chat messages the upstream saw, including custom calls and
// namespaced functions.
func TestCodexHistoryItemsDecode(t *testing.T) {
	payload := codexPayload()
	payload["input"] = []any{
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "patch it"}}},
		map[string]any{"type": "custom_tool_call", "call_id": "call_ct", "name": "apply_patch", "input": "*** Begin Patch"},
		map[string]any{"type": "custom_tool_call_output", "call_id": "call_ct", "output": "done"},
		map[string]any{"type": "function_call", "call_id": "call_ns", "name": "close_agent", "namespace": "multi_agent_v1", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "call_ns", "output": "closed"},
		map[string]any{"type": "web_search_call", "id": "ws_1", "status": "completed"},
	}
	prepared, err := PrepareRequest(Responses, Chat, payload, "https://upstream.example")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	body, _ := json.Marshal(prepared)
	text := string(body)
	for _, want := range []string{"apply_patch", "multi_agent_v1__close_agent", "Begin Patch", "closed"} {
		if !strings.Contains(text, want) {
			t.Fatalf("history decode missing %q: %s", want, text[:600])
		}
	}
}

func TestFlattenNamespaceToolNameStaysWithinLimit(t *testing.T) {
	long := strings.Repeat("n", 60)
	flattened := flattenNamespaceToolName(long, "child")
	if len(flattened) > chatToolNameMaxLen {
		t.Fatalf("flattened name too long (%d): %s", len(flattened), flattened)
	}
	if !strings.HasPrefix(flattened, "n") || !strings.Contains(flattened, "__") {
		t.Fatalf("unexpected flattened name: %s", flattened)
	}
	if again := flattenNamespaceToolName(long, "child"); again != flattened {
		t.Fatalf("flattening must be deterministic: %s vs %s", flattened, again)
	}
}
