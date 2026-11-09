// arachne-mcp brings arachne's MCP server to agents that only start local
// commands: it speaks MCP over stdin and stdout and passes every call on to
// the server's /mcp, signed in with the user's API token from Settings.
//
//	ARACHNE_URL=http://localhost:8080 ARACHNE_TOKEN=arachne_... arachne-mcp
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const defaultURL = "http://localhost:8080"

func main() {
	base, token := os.Getenv("ARACHNE_URL"), os.Getenv("ARACHNE_TOKEN")
	if base == "" {
		base = defaultURL
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Stdout is the protocol's: anything else goes to stderr, which agents log.
	if err := run(ctx, base, token, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "arachne-mcp:", err)
		os.Exit(1)
	}
}

type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))

	return http.DefaultTransport.RoundTrip(r)
}

// run serves the server's tools, as the server lists them, on local.
func run(ctx context.Context, base, token string, local mcp.Transport) error {
	if token == "" {
		return errors.New("ARACHNE_TOKEN is not set: make a token on arachne's Settings page")
	}

	relay := &progressRelay{waiting: map[any]*mcp.ServerSession{}}

	client := mcp.NewClient(&mcp.Implementation{Name: "arachne-mcp", Version: "1"}, &mcp.ClientOptions{
		ProgressNotificationHandler: relay.forward,
	})

	remote, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   strings.TrimRight(base, "/") + "/mcp",
		HTTPClient: &http.Client{Transport: bearer(token)},
	}, nil)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", base, err)
	}
	defer remote.Close()

	var instructions string
	if init := remote.InitializeResult(); init != nil {
		instructions = init.Instructions
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "arachne", Title: "arachne", Version: "1"}, &mcp.ServerOptions{Instructions: instructions})

	for tool, err := range remote.Tools(ctx, nil) {
		if err != nil {
			return fmt.Errorf("listing the tools of %s: %w", base, err)
		}

		server.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			params := &mcp.CallToolParams{Name: req.Params.Name, Arguments: req.Params.Arguments}

			if token := req.Params.GetProgressToken(); relay.add(token, req.Session) {
				defer relay.release(token)

				params.SetProgressToken(token)
			}

			return remote.CallTool(ctx, params)
		})
	}

	return server.Run(ctx, local)
}

// progressRelay passes the server's progress on a call to the agent that
// made it, under the agent's own token.
type progressRelay struct {
	mu      sync.Mutex
	waiting map[any]*mcp.ServerSession
}

// The client may handle a call's last notifications after its result: a
// token is kept this long after its call, so they still reach the agent.
const progressGrace = 5 * time.Second

// add reports whether progress on token can be relayed: a token is a string
// or a number, and stays its call's until a while after that ends.
func (p *progressRelay) add(token any, session *mcp.ServerSession) bool {
	switch token.(type) {
	case string, float64, int64, int:
	default:
		return false
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if _, taken := p.waiting[token]; taken || session == nil {
		return false
	}

	p.waiting[token] = session

	return true
}

func (p *progressRelay) release(token any) {
	time.AfterFunc(progressGrace, func() {
		p.mu.Lock()
		defer p.mu.Unlock()

		delete(p.waiting, token)
	})
}

func (p *progressRelay) forward(ctx context.Context, req *mcp.ProgressNotificationClientRequest) {
	p.mu.Lock()
	session := p.waiting[req.Params.ProgressToken]
	p.mu.Unlock()

	if session != nil {
		_ = session.NotifyProgress(ctx, req.Params)
	}
}
