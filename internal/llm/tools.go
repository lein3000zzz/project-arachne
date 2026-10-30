package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// toolCalls assembles calls streamed in pieces: the name and ID come first,
// the arguments as fragments of a JSON string, all keyed by the call's index.
type toolCalls struct {
	order []int
	calls map[int]*partialCall
}

type partialCall struct {
	id, name string
	args     strings.Builder
}

func (t *toolCalls) add(index int, id, name, args string) {
	if t.calls == nil {
		t.calls = map[int]*partialCall{}
	}

	call, ok := t.calls[index]
	if !ok {
		call = &partialCall{}
		t.calls[index] = call
		t.order = append(t.order, index)
	}

	if id != "" {
		call.id = id
	}

	if name != "" {
		call.name = name
	}

	call.args.WriteString(args)
}

func (t *toolCalls) done() []ToolCall {
	var out []ToolCall

	for i, index := range t.order {
		call := t.calls[index]
		if call.name == "" {
			continue
		}

		id := call.id
		if id == "" {
			id = fmt.Sprintf("call_%d", i)
		}

		out = append(out, ToolCall{ID: id, Name: call.name, Arguments: arguments(json.RawMessage(call.args.String()))})
	}

	return out
}

// arguments is the call's arguments as a JSON object: a model that called a tool
// with nothing sends "" or null, which no handler should have to special-case.
func arguments(raw json.RawMessage) json.RawMessage {
	if s := strings.TrimSpace(string(raw)); s == "" || s == "null" {
		return json.RawMessage("{}")
	}

	return raw
}

// ToolHandler runs one call and returns what the model is told; done ends the
// conversation after this call.
type ToolHandler func(ctx context.Context, call ToolCall) (result string, done bool)

type ToolRun struct {
	// The whole conversation, the request's own messages first.
	Messages []Message
	Last     Response
	Calls    int
	// A handler ended the conversation.
	Done  bool
	Usage Usage
}

// RunTools sends req and runs the tools the replies call, sending each result
// back, until a reply calls none, a handler is done, or maxCalls tools have
// run. A call past the budget is answered without running; the caller decides
// what a run that ended that way is worth.
func RunTools(ctx context.Context, c Client, req Request, maxCalls int, handle ToolHandler) (ToolRun, error) {
	run := ToolRun{Messages: slices.Clone(req.Messages)}

	for {
		next := req
		next.Messages = run.Messages

		resp, err := c.Stream(ctx, next, nil)
		run.Usage.InputTokens += resp.Usage.InputTokens
		run.Usage.OutputTokens += resp.Usage.OutputTokens
		run.Last = resp

		if err != nil {
			return run, err
		}

		// Calls a small model wrote out as text, in its native JSON shape but
		// without the markers the server parses: run, and sent back as real
		// calls, which also shows the model the form that works.
		if len(resp.ToolCalls) == 0 && len(req.Tools) > 0 {
			if written := textToolCalls(resp.Text, req.Tools); len(written) > 0 {
				resp.Text, resp.ToolCalls = "", written
				run.Last = resp
			}
		}

		run.Messages = append(run.Messages, Message{Role: RoleAssistant, Content: resp.Text, ToolCalls: resp.ToolCalls})

		if len(resp.ToolCalls) == 0 {
			return run, nil
		}

		for _, call := range resp.ToolCalls {
			result := "Not run: no tool calls are left."

			if run.Calls < maxCalls {
				var done bool
				if result, done = handle(ctx, call); done {
					run.Done = true
				}
			}

			run.Calls++
			run.Messages = append(run.Messages, Message{Role: RoleTool, Content: result, ToolCallID: call.ID})

			if run.Done {
				return run, nil
			}
		}

		if run.Calls >= maxCalls {
			return run, nil
		}
	}
}

// textToolCalls finds {"name": ..., "arguments": {...}} objects naming one of
// the tools anywhere in text - "parameters" is accepted for "arguments".
func textToolCalls(text string, tools []Tool) []ToolCall {
	known := make(map[string]bool, len(tools))
	for _, t := range tools {
		known[t.Name] = true
	}

	var calls []ToolCall

	for i := 0; i < len(text); i++ {
		if text[i] != '{' {
			continue
		}

		dec := json.NewDecoder(strings.NewReader(text[i:]))

		var obj struct {
			Name       string          `json:"name"`
			Arguments  json.RawMessage `json:"arguments"`
			Parameters json.RawMessage `json:"parameters"`
		}

		if err := dec.Decode(&obj); err != nil || !known[obj.Name] {
			continue
		}

		args := obj.Arguments
		if len(args) == 0 {
			args = obj.Parameters
		}

		if strings.HasPrefix(strings.TrimSpace(string(args)), "{") || len(args) == 0 {
			calls = append(calls, ToolCall{ID: fmt.Sprintf("text_%d", len(calls)), Name: obj.Name, Arguments: arguments(args)})
		}

		i += int(dec.InputOffset()) - 1
	}

	return calls
}
