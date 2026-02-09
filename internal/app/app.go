package app

import "context"

type App interface {
	StartApp() error
	StopApp(ctx context.Context) error
}
