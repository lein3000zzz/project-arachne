package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var lookup = Tool{Name: "lookup", Description: "look a word up", Parameters: json.RawMessage(`{"type":"object","properties":{"word":{"type":"string"}}}`)}

func TestOpenAIToolCallsRoundTrip(t *testing.T) {
	var got map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		// The shape OpenAI streams: name and id first, arguments in fragments;
		// Ollama sends a whole call in one chunk, which this covers too.
		sse(w,
			`data: {"choices":[{"delta":{"content":"Let me check."}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"lookup","arguments":""}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"word\":"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"go\"}"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"lookup","arguments":""}}]}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`)
	}))
	defer srv.Close()

	req := Request{
		Tools: []Tool{lookup},
		Messages: []Message{
			{Role: RoleUser, Content: "what is go?"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call_0", Name: "lookup", Arguments: json.RawMessage(`{"word":"go"}`)}}},
			{Role: RoleTool, ToolCallID: "call_0", Content: "a language"},
		},
	}

	resp, deltas, err := collect(t, newClient(t, ProviderOpenAICompatible, srv.URL), req)
	if err != nil {
		t.Fatal(err)
	}

	if len(resp.ToolCalls) != 2 || resp.ToolCalls[0].ID != "call_a" || string(resp.ToolCalls[0].Arguments) != `{"word":"go"}` ||
		resp.ToolCalls[1].ID != "call_b" || string(resp.ToolCalls[1].Arguments) != `{}` || resp.StopReason != "tool_calls" {
		t.Errorf("tool calls = %+v, stop %q", resp.ToolCalls, resp.StopReason)
	}

	if resp.Text != "Let me check." || strings.Join(deltas, "") != "Let me check." {
		t.Errorf("text %q, deltas %q", resp.Text, deltas)
	}

	tools, _ := got["tools"].([]any)
	if fn, _ := tools[0].(map[string]any)["function"].(map[string]any); len(tools) != 1 || fn["name"] != "lookup" || fn["parameters"] == nil {
		t.Errorf("tools sent: %v", got["tools"])
	}

	msgs, _ := got["messages"].([]any)
	assistant, _ := msgs[1].(map[string]any)
	call, _ := assistant["tool_calls"].([]any)[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	tool, _ := msgs[2].(map[string]any)

	if call["id"] != "call_0" || call["type"] != "function" || fn["arguments"] != `{"word":"go"}` ||
		tool["role"] != "tool" || tool["tool_call_id"] != "call_0" || tool["content"] != "a language" {
		t.Errorf("messages sent: %v", got["messages"])
	}
}

func TestAnthropicToolCallsRoundTrip(t *testing.T) {
	var got map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		sse(w,
			`event: message_start`+"\n"+`data: {"type":"message_start","message":{"usage":{"input_tokens":30}}}`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Checking."}}`,
			`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}}`,
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"wo"}}`,
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"rd\": \"go\"}"}}`,
			`data: {"type":"content_block_stop","index":1}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":12}}`,
			`data: {"type":"message_stop"}`)
	}))
	defer srv.Close()

	req := Request{
		Tools: []Tool{lookup},
		Messages: []Message{
			{Role: RoleUser, Content: "what is go?"},
			{Role: RoleAssistant, Content: "Looking.", ToolCalls: []ToolCall{{ID: "t1", Name: "lookup", Arguments: json.RawMessage(`{"word":"go"}`)},
				{ID: "t2", Name: "lookup", Arguments: json.RawMessage(`{"word":"gopher"}`)}}},
			{Role: RoleTool, ToolCallID: "t1", Content: "a language"},
			{Role: RoleTool, ToolCallID: "t2", Content: "a mascot"},
			{Role: RoleUser, Content: "decide now"},
		},
	}

	resp, _, err := collect(t, newClient(t, ProviderAnthropic, srv.URL), req)
	if err != nil {
		t.Fatal(err)
	}

	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "toolu_1" || resp.ToolCalls[0].Name != "lookup" ||
		string(resp.ToolCalls[0].Arguments) != `{"word": "go"}` || resp.Text != "Checking." || resp.StopReason != "tool_use" {
		t.Errorf("response = %+v", resp)
	}

	if tools, _ := got["tools"].([]any); len(tools) != 1 || tools[0].(map[string]any)["input_schema"] == nil {
		t.Errorf("tools sent: %v", got["tools"])
	}

	msgs, _ := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("%d messages sent, want 3: the results and the note merge into one user turn: %v", len(msgs), msgs)
	}

	assistant := msgs[1].(map[string]any)["content"].([]any)
	results := msgs[2].(map[string]any)["content"].([]any)

	if len(assistant) != 3 || assistant[1].(map[string]any)["type"] != "tool_use" || assistant[1].(map[string]any)["id"] != "t1" ||
		len(results) != 3 || results[0].(map[string]any)["tool_use_id"] != "t1" || results[1].(map[string]any)["content"] != "a mascot" ||
		results[2].(map[string]any)["text"] != "decide now" {
		t.Errorf("messages sent: %v", msgs)
	}
}

// scripted replies with the next response each time it is called.
type scripted struct {
	replies  []Response
	requests []Request
}

func (s *scripted) Model() string { return "scripted" }

func (s *scripted) Stream(_ context.Context, req Request, _ func(string) error) (Response, error) {
	s.requests = append(s.requests, req)
	resp := s.replies[0]

	if len(s.replies) > 1 {
		s.replies = s.replies[1:]
	}

	return resp, nil
}

func call(id, name string) ToolCall {
	return ToolCall{ID: id, Name: name, Arguments: json.RawMessage(`{}`)}
}

func TestRunToolsUntilTheModelStops(t *testing.T) {
	c := &scripted{replies: []Response{
		{ToolCalls: []ToolCall{call("1", "lookup"), call("2", "lookup")}, Usage: Usage{InputTokens: 10, OutputTokens: 2}},
		{Text: "done", Usage: Usage{InputTokens: 15, OutputTokens: 1}},
	}}

	var ran []string

	run, err := RunTools(context.Background(), c, Request{Messages: []Message{{Role: RoleUser, Content: "q"}}}, 5,
		func(_ context.Context, call ToolCall) (string, bool) {
			ran = append(ran, call.ID)
			return "result " + call.ID, false
		})
	if err != nil {
		t.Fatal(err)
	}

	if run.Calls != 2 || run.Done || run.Last.Text != "done" || run.Usage != (Usage{InputTokens: 25, OutputTokens: 3}) || strings.Join(ran, ",") != "1,2" {
		t.Errorf("run = %+v, ran %v", run, ran)
	}

	second := c.requests[1].Messages
	if len(second) != 4 || second[1].Role != RoleAssistant || len(second[1].ToolCalls) != 2 ||
		second[2].ToolCallID != "1" || second[3].Content != "result 2" {
		t.Errorf("the second request must carry the calls and their results: %+v", second)
	}
}

func TestRunToolsStopsAtTheBudgetOrWhenDone(t *testing.T) {
	looping := &scripted{replies: []Response{{ToolCalls: []ToolCall{call("x", "lookup"), call("y", "lookup")}}}}

	handled := 0

	run, err := RunTools(context.Background(), looping, Request{}, 3, func(context.Context, ToolCall) (string, bool) {
		handled++
		return "r", false
	})
	if err != nil || handled != 3 || run.Calls != 4 || len(looping.requests) != 2 || run.Done {
		t.Errorf("handled %d, calls %d, requests %d, %v: three run, the fourth answered unrun, then stop", handled, run.Calls, len(looping.requests), err)
	}

	if last := run.Messages[len(run.Messages)-1]; !strings.Contains(last.Content, "no tool calls are left") {
		t.Errorf("the call past the budget was answered with %q", last.Content)
	}

	finishing := &scripted{replies: []Response{{ToolCalls: []ToolCall{call("v", "verdict"), call("w", "lookup")}}}}

	run, _ = RunTools(context.Background(), finishing, Request{}, 5, func(_ context.Context, c ToolCall) (string, bool) {
		return "ok", c.Name == "verdict"
	})
	if !run.Done || run.Calls != 1 || len(finishing.requests) != 1 {
		t.Errorf("done: %+v", run)
	}
}

func TestCallsWrittenAsTextAreRun(t *testing.T) {
	text := `addError {"name": "add_source", "arguments": {"url": "https://a.example/", "reason": "x {y}"}} then ` +
		`{"name":"lookup","parameters":{"word":"go"}} and {"name": "unknown", "arguments": {}} and {"not": "a call"} ` +
		`and a broken {"name": "lookup", "arguments": {`

	calls := textToolCalls(text, []Tool{lookup, {Name: "add_source"}})
	if len(calls) != 2 || calls[0].Name != "add_source" || string(calls[0].Arguments) != `{"url": "https://a.example/", "reason": "x {y}"}` ||
		calls[1].Name != "lookup" || string(calls[1].Arguments) != `{"word":"go"}` || calls[0].ID == calls[1].ID {
		t.Errorf("calls = %+v", calls)
	}

	c := &scripted{replies: []Response{
		{Text: `{"name": "lookup", "arguments": {"word": "go"}}`},
		{Text: "done"},
	}}

	var ran []string

	run, err := RunTools(context.Background(), c, Request{Tools: []Tool{lookup}}, 5, func(_ context.Context, call ToolCall) (string, bool) {
		ran = append(ran, string(call.Arguments))
		return "a language", false
	})
	if err != nil || len(ran) != 1 || run.Calls != 1 {
		t.Fatalf("ran %v, %+v, %v", ran, run, err)
	}

	sent := c.requests[1].Messages
	if sent[0].Content != "" || len(sent[0].ToolCalls) != 1 || sent[1].Role != RoleTool {
		t.Errorf("sent back %+v: the written call goes back as a real one", sent)
	}

	if calls := textToolCalls(`{"name": "lookup"}`, []Tool{lookup}); len(calls) != 1 || string(calls[0].Arguments) != "{}" {
		t.Errorf("no arguments: %+v", calls)
	}

	if calls := textToolCalls("plain prose about lookup", []Tool{lookup}); len(calls) != 0 {
		t.Errorf("prose: %+v", calls)
	}
}
