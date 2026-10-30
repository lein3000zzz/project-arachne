package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const anthropicVersion = "2023-06-01"

// Anthropic requires max_tokens on every request.
const anthropicDefaultMaxTokens = 1024

type anthropicClient struct {
	transport
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicRequest struct {
	Model       string             `json:"model"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	MaxTokens   int                `json:"max_tokens"`
	Temperature *float64           `json:"temperature,omitempty"`
	Stream      bool               `json:"stream"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
}

type anthropicEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message struct {
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *anthropicClient) Stream(ctx context.Context, req Request, onDelta func(string) error) (Response, error) {
	body := anthropicRequest{
		Model:       c.cfg.Model,
		System:      req.System,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		Stream:      true,
	}

	if body.MaxTokens <= 0 {
		body.MaxTokens = anthropicDefaultMaxTokens
	}

	body.Messages = anthropicMessages(req.Messages)

	for _, tool := range req.Tools {
		body.Tools = append(body.Tools, anthropicTool{Name: tool.Name, Description: tool.Description, InputSchema: tool.Parameters})
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, err
	}

	return c.withRetries(ctx, onDelta, func(ctx context.Context, onDelta func(string) error) (Response, bool, bool, error) {
		return c.attempt(ctx, payload, onDelta)
	})
}

func (c *anthropicClient) attempt(ctx context.Context, payload []byte, onDelta func(string) error) (Response, bool, bool, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/messages", bytes.NewReader(payload))
	if err != nil {
		return Response{}, false, false, err
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("X-Api-Key", c.cfg.APIKey)
	httpReq.Header.Set("Anthropic-Version", anthropicVersion)

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return Response{}, false, true, fmt.Errorf("%w: %w", ErrRequest, err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(httpResp.Body, errorBodyLimit))
		// 529 is Anthropic's "overloaded", worth another try like a 5xx.
		return Response{}, false, retryableStatus(httpResp.StatusCode),
			fmt.Errorf("%w: HTTP %d: %s", ErrRequest, httpResp.StatusCode, truncate(body))
	}

	var (
		resp     Response
		text     strings.Builder
		calls    toolCalls
		streamed bool
		finished bool
	)

	err = readSSE(httpResp.Body, func(e sseEvent) (bool, error) {
		var event anthropicEvent
		if err := json.Unmarshal([]byte(e.data), &event); err != nil {
			return false, fmt.Errorf("%w: %w", ErrMalformed, err)
		}

		switch event.Type {
		case "message_start":
			resp.Usage.InputTokens = event.Message.Usage.InputTokens
		case "content_block_start":
			if event.ContentBlock.Type == "tool_use" {
				calls.add(event.Index, event.ContentBlock.ID, event.ContentBlock.Name, "")
			}
		case "content_block_delta":
			if event.Delta.Type == "input_json_delta" {
				calls.add(event.Index, "", "", event.Delta.PartialJSON)
				return false, nil
			}

			if event.Delta.Type != "text_delta" || event.Delta.Text == "" {
				return false, nil
			}

			streamed = true
			text.WriteString(event.Delta.Text)

			return false, onDelta(event.Delta.Text)
		case "message_delta":
			resp.StopReason = event.Delta.StopReason
			resp.Usage.OutputTokens = event.Usage.OutputTokens
		case "message_stop":
			finished = true
			return true, nil
		case "error":
			return false, fmt.Errorf("%w: %s: %s", ErrRequest, event.Error.Type, event.Error.Message)
		}

		return false, nil
	})

	resp.Text = text.String()
	resp.ToolCalls = calls.done()

	if err == nil && !finished {
		err = fmt.Errorf("%w: the stream ended before the reply did", ErrRequest)
	}

	if err != nil {
		return resp, streamed, !streamed && ctx.Err() == nil, err
	}

	return resp, streamed, false, nil
}

// anthropicMessages turns messages into the Messages API's blocks. Tool results
// travel in user turns, and the API wants turns to alternate, so consecutive
// user-side messages - several results, or results then a note - merge into one.
func anthropicMessages(messages []Message) []anthropicMessage {
	var out []anthropicMessage

	for _, m := range messages {
		role := string(m.Role)

		var blocks []anthropicBlock

		switch m.Role {
		case RoleTool:
			role = string(RoleUser)
			blocks = []anthropicBlock{{Type: "tool_result", ToolUseID: m.ToolCallID, Content: m.Content}}
		default:
			if m.Content != "" || len(m.ToolCalls) == 0 {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: m.Content})
			}

			for _, call := range m.ToolCalls {
				blocks = append(blocks, anthropicBlock{Type: "tool_use", ID: call.ID, Name: call.Name, Input: arguments(call.Arguments)})
			}
		}

		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			continue
		}

		out = append(out, anthropicMessage{Role: role, Content: blocks})
	}

	return out
}
