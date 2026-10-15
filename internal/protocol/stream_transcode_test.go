package protocol

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// chatSSE mirrors what the upstream Chat lane sends: a role chunk, two text
// deltas, a finish chunk, and the [DONE] terminator.
const chatSSE = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" world\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\n\n" +
	"data: [DONE]\n\n"

const chatToolSSE = "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"\"}}]}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"city\\\":\"}}]}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"Beijing\\\"}\"}}]}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
	"data: [DONE]\n\n"

const anthropicSSE = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"m\",\"usage\":{\"input_tokens\":7,\"output_tokens\":0}}}\n\n" +
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

func TestTranscodeChatToAnthropic(t *testing.T) {
	recorder := httptest.NewRecorder()
	usage, reported, err := TranscodeStream(context.Background(), recorder, strings.NewReader(chatSSE), Chat, Anthropic, "m")
	if err != nil {
		t.Fatalf("transcode: %v", err)
	}
	if !reported || usage.Input == 0 && usage.Output == 0 {
		t.Fatalf("usage not reported: %+v reported=%v", usage, reported)
	}
	body := recorder.Body.String()
	for _, want := range []string{"message_start", "content_block_delta", "text_delta", "\"Hello\"", "\" world\"", "message_stop"} {
		if !strings.Contains(body, want) {
			t.Fatalf("anthropic stream missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("Anthropic stream must not carry [DONE]:\n%s", body)
	}
}

func TestTranscodeChatToolCallToResponses(t *testing.T) {
	recorder := httptest.NewRecorder()
	if _, _, err := TranscodeStream(context.Background(), recorder, strings.NewReader(chatToolSSE), Chat, Responses, "m"); err != nil {
		t.Fatalf("transcode: %v", err)
	}
	body := recorder.Body.String()
	for _, want := range []string{"response.output_item.added", "function_call", "get_weather", "response.function_call_arguments.delta", "response.function_call_arguments.done", "Beijing", "response.completed"} {
		if !strings.Contains(body, want) {
			t.Fatalf("responses stream missing %q:\n%s", want, body)
		}
	}
}

func TestTranscodeAnthropicToChat(t *testing.T) {
	recorder := httptest.NewRecorder()
	if _, _, err := TranscodeStream(context.Background(), recorder, strings.NewReader(anthropicSSE), Anthropic, Chat, "m"); err != nil {
		t.Fatalf("transcode: %v", err)
	}
	body := recorder.Body.String()
	for _, want := range []string{"chat.completion.chunk", "\"Hi\"", "[DONE]"} {
		if !strings.Contains(body, want) {
			t.Fatalf("chat stream missing %q:\n%s", want, body)
		}
	}
}

func TestTranscodeChatTerminationDiscipline(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantError bool
		want      []string
	}{
		{
			name:      "truncated stream emits a terminal error event",
			input:     "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n",
			wantError: true,
			want:      []string{"partial", "\"error\"", "[DONE]"},
		},
		{
			name:      "upstream error frame is forwarded",
			input:     "data: {\"error\":{\"message\":\"boom\",\"type\":\"server_error\"}}\n\n",
			wantError: true,
			want:      []string{"boom", "[DONE]"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			_, _, err := TranscodeStream(context.Background(), recorder, strings.NewReader(testCase.input), Chat, Chat, "m")
			if testCase.wantError && err == nil {
				t.Fatalf("expected an error for input %q", testCase.input)
			}
			if !testCase.wantError && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			body := recorder.Body.String()
			for _, want := range testCase.want {
				if !strings.Contains(body, want) {
					t.Fatalf("stream missing %q:\n%s", want, body)
				}
			}
		})
	}
}

func TestCollapseStreamChat(t *testing.T) {
	collapsed, err := CollapseStream(strings.NewReader(chatSSE), Chat, "m")
	if err != nil {
		t.Fatalf("collapse: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(collapsed, &document); err != nil {
		t.Fatalf("collapsed document is not JSON: %v", err)
	}
	choices, _ := document["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %v", document["choices"])
	}
	message, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "Hello world" {
		t.Fatalf("collapsed content = %v", message["content"])
	}
	if _, err := CollapseStream(strings.NewReader("data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n"), Chat, "m"); err == nil {
		t.Fatal("a truncated stream must not collapse into a document")
	}
}

func TestStreamHeadInspector(t *testing.T) {
	cases := []struct {
		name   string
		frames []string
		want   StreamHeadClass
	}{
		{"role chunk is not content", []string{"{\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}"}, StreamHeadNone},
		{"text delta is content", []string{"{\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}"}, StreamHeadContent},
		{"reasoning delta is content", []string{"{\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"think\"}}]}"}, StreamHeadContent},
		{"done without content is terminal", []string{"[DONE]"}, StreamHeadTerminal},
		{"finish without content is terminal", []string{"{\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}"}, StreamHeadTerminal},
		{"error frame is an error", []string{"{\"error\":{\"message\":\"boom\"}}"}, StreamHeadError},
		{"garbage is an error", []string{"not json"}, StreamHeadError},
		{"usage only is nothing decisive", []string{"{\"id\":\"c\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":0,\"total_tokens\":1}}"}, StreamHeadNone},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			inspector := NewStreamHeadInspector(Chat)
			class := StreamHeadNone
			for _, frame := range testCase.frames {
				class = inspector.Feed("", frame)
			}
			if class != testCase.want {
				t.Fatalf("class = %v, want %v", class, testCase.want)
			}
		})
	}
}

func TestHoldFirstContent(t *testing.T) {
	t.Run("content releases the prefix untouched", func(t *testing.T) {
		stream := ": comment\n\n" + "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n" +
			"data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
			"data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"!!\"}}]}\n\ndata: [DONE]\n\n"
		prefix, class, err := HoldFirstContent(strings.NewReader(stream), Chat, nil)
		if err != nil {
			t.Fatalf("hold: %v", err)
		}
		if class != StreamHeadContent {
			t.Fatalf("class = %v", class)
		}
		if !strings.Contains(string(prefix), "\"hi\"") {
			t.Fatalf("prefix must contain the content frame: %q", prefix)
		}
		// The prefix is everything consumed so far; the caller replays it, so
		// no byte may be lost or duplicated.
		if !strings.HasPrefix(stream, string(prefix)) {
			t.Fatalf("prefix is not a byte prefix of the stream: %q", prefix)
		}
		if len(prefix) > len(stream) {
			t.Fatalf("prefix longer than stream")
		}
	})

	t.Run("done only classifies terminal", func(t *testing.T) {
		stream := "data: [DONE]\n\n"
		prefix, class, err := HoldFirstContent(strings.NewReader(stream), Chat, nil)
		if err != nil {
			t.Fatalf("hold: %v", err)
		}
		if class != StreamHeadTerminal || string(prefix) != stream {
			t.Fatalf("class=%v prefix=%q", class, prefix)
		}
	})

	t.Run("eof after a contentless frame is a clean zero-output end", func(t *testing.T) {
		prefix, class, err := HoldFirstContent(strings.NewReader("data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n"), Chat, nil)
		if err != nil {
			t.Fatalf("a completed frame must not report a stream error: %v", err)
		}
		if class != StreamHeadNone {
			t.Fatalf("class = %v", class)
		}
		if !strings.Contains(string(prefix), "assistant") {
			t.Fatalf("prefix lost the consumed frame: %q", prefix)
		}
	})

	t.Run("unterminated frame at EOF reports the stream error", func(t *testing.T) {
		_, _, err := HoldFirstContent(strings.NewReader("data: {\"id\":\"c\"}"), Chat, nil)
		if err == nil {
			t.Fatal("an unterminated frame must report a stream error")
		}
	})

	t.Run("onRead fires for every read", func(t *testing.T) {
		reads := 0
		_, _, _ = HoldFirstContent(strings.NewReader(chatSSE), Chat, func() { reads++ })
		if reads == 0 {
			t.Fatal("onRead never fired")
		}
	})
}
