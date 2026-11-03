package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// remote stands in for arachne's /mcp: one tool that reports progress, behind
// a token.
func remote(t *testing.T) *httptest.Server {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "arachne", Version: "1"}, &mcp.ServerOptions{Instructions: "answers with citations"})

	type in struct {
		Question string `json:"question"`
	}

	mcp.AddTool(server, &mcp.Tool{Name: "ask", Description: "answer"}, func(ctx context.Context, req *mcp.CallToolRequest, q in) (*mcp.CallToolResult, any, error) {
		if token := req.Params.GetProgressToken(); token != nil {
			_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: token, Progress: 1, Message: "reading the sources"})
		}

		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "answered: " + q.Question}}}, nil, nil
	})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer arachne_good" {
			http.Error(w, "sign in", http.StatusUnauthorized)
			return
		}

		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	return srv
}

func TestCallsArePassedOnWithTheirProgress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agentSide, ourSide := mcp.NewInMemoryTransports()

	done := make(chan error, 1)
	go func() { done <- run(ctx, remote(t).URL+"/", "arachne_good", ourSide) }()

	var (
		mu    sync.Mutex
		heard []string
	)

	agent := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "1"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			mu.Lock()
			defer mu.Unlock()

			heard = append(heard, req.Params.Message)
		},
	})

	session, err := agent.Connect(ctx, agentSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	if got := session.InitializeResult().Instructions; got != "answers with citations" {
		t.Errorf("instructions %q", got)
	}

	params := &mcp.CallToolParams{Name: "ask", Arguments: map[string]any{"question": "zig?"}}
	params.SetProgressToken("p1")

	res, err := session.CallTool(ctx, params)
	if err != nil || len(res.Content) != 1 || res.Content[0].(*mcp.TextContent).Text != "answered: zig?" {
		t.Fatalf("call: %+v, %v", res, err)
	}

	deadline := time.Now().Add(time.Second)
	for {
		mu.Lock()
		got := slices.Clone(heard)
		mu.Unlock()

		if slices.Equal(got, []string{"reading the sources"}) {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("progress heard: %q", got)
		}

		time.Sleep(10 * time.Millisecond)
	}

	_ = session.Close()
	cancel()

	if err := <-done; err != nil && ctx.Err() == nil {
		t.Errorf("run: %v", err)
	}
}

func TestAWrongTokenIsSaidAtStart(t *testing.T) {
	_, ourSide := mcp.NewInMemoryTransports()

	if err := run(context.Background(), remote(t).URL, "arachne_bad", ourSide); err == nil || !strings.Contains(err.Error(), "connecting to") {
		t.Errorf("err = %v", err)
	}

	if err := run(context.Background(), remote(t).URL, "", ourSide); err == nil || !strings.Contains(err.Error(), "Settings") {
		t.Errorf("no token: %v", err)
	}
}
