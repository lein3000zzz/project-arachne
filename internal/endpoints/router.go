package endpoints

import (
	"context"
	"web-crawler/internal/auth"
	"web-crawler/internal/llm"
)

// Clients gives a user's own model, if they set one.
type Clients interface {
	Client(ctx context.Context, userID string) (llm.Client, bool, error)
}

// Router is a chat model that answers with the asking user's own endpoint
// when they set one, else with the server's model - which may be none. Who is
// asking comes from the context: the signed-in user of a request, or the owner
// a background job runs for.
type Router struct {
	own      Clients
	fallback llm.Client
}

// NewRouter takes nil own when users cannot bring endpoints, and nil fallback
// when the server runs no chat model.
func NewRouter(own Clients, fallback llm.Client) *Router {
	return &Router{own: own, fallback: fallback}
}

func (r *Router) Stream(ctx context.Context, req llm.Request, onDelta func(string) error) (llm.Response, error) {
	c, err := r.pick(ctx)
	if err != nil {
		return llm.Response{}, err
	}

	return c.Stream(ctx, req, onDelta)
}

// Model is the server's model; ModelFor names the one a request gets.
func (r *Router) Model() string {
	if r.fallback == nil {
		return ""
	}

	return r.fallback.Model()
}

func (r *Router) ModelFor(ctx context.Context) string {
	c, err := r.pick(ctx)
	if err != nil {
		return ""
	}

	return c.Model()
}

func (r *Router) pick(ctx context.Context) (llm.Client, error) {
	if user, ok := auth.UserFrom(ctx); ok && r.own != nil {
		c, ok, err := r.own.Client(ctx, user.ID)
		if err != nil {
			return nil, err
		}

		if ok {
			return c, nil
		}
	}

	if r.fallback == nil {
		return nil, llm.ErrNoModel
	}

	return r.fallback, nil
}
