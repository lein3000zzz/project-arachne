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

type openAIClient struct {
	transport

	// OpenAI's own reasoning models reject max_tokens and want this instead;
	// most compatible servers only know max_tokens.
	completionTokens bool
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIRequest struct {
	Model               string          `json:"model"`
	Messages            []openAIMessage `json:"messages"`
	MaxTokens           int             `json:"max_tokens,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	Stream              bool            `json:"stream"`
	StreamOptions       struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

type openAIChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (c *openAIClient) Stream(ctx context.Context, req Request, onDelta func(string) error) (Response, error) {
	body := openAIRequest{Model: c.cfg.Model, Temperature: req.Temperature, Stream: true}
	body.StreamOptions.IncludeUsage = true

	if c.completionTokens {
		body.MaxCompletionTokens = req.MaxTokens
	} else {
		body.MaxTokens = req.MaxTokens
	}

	if req.System != "" {
		body.Messages = append(body.Messages, openAIMessage{Role: "system", Content: req.System})
	}

	for _, m := range req.Messages {
		body.Messages = append(body.Messages, openAIMessage{Role: string(m.Role), Content: m.Content})
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, err
	}

	return c.withRetries(ctx, onDelta, func(ctx context.Context, onDelta func(string) error) (Response, bool, bool, error) {
		return c.attempt(ctx, payload, onDelta)
	})
}

func (c *openAIClient) attempt(ctx context.Context, payload []byte, onDelta func(string) error) (Response, bool, bool, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return Response{}, false, false, err
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	if c.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return Response{}, false, true, fmt.Errorf("%w: %w", ErrRequest, err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(httpResp.Body, errorBodyLimit))
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
		if e.data == "[DONE]" {
			finished = true
			return true, nil
		}

		var chunk openAIChunk
		if err := json.Unmarshal([]byte(e.data), &chunk); err != nil {
			return false, fmt.Errorf("%w: %w", ErrMalformed, err)
		}

		if chunk.Error != nil {
			return false, fmt.Errorf("%w: %s", ErrRequest, chunk.Error.Message)
		}

		if chunk.Usage != nil {
			resp.Usage = Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
		}

		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil {
				resp.StopReason = *choice.FinishReason
			}

			if choice.Delta.Content == "" {
				continue
			}

			streamed = true
			text.WriteString(choice.Delta.Content)

			if err := onDelta(choice.Delta.Content); err != nil {
				return false, err
			}
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
