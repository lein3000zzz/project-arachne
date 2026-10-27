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
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicRequest struct {
	Model       string             `json:"model"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	MaxTokens   int                `json:"max_tokens"`
	Temperature *float64           `json:"temperature,omitempty"`
	Stream      bool               `json:"stream"`
}

type anthropicEvent struct {
	Type    string `json:"type"`
	Message struct {
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Delta struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		StopReason string `json:"stop_reason"`
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

	for _, m := range req.Messages {
		body.Messages = append(body.Messages, anthropicMessage{Role: string(m.Role), Content: m.Content})
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
		case "content_block_delta":
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

	if err == nil && !finished {
		err = fmt.Errorf("%w: the stream ended before the reply did", ErrRequest)
	}

	if err != nil {
		return resp, streamed, !streamed && ctx.Err() == nil, err
	}

	return resp, streamed, false, nil
}
