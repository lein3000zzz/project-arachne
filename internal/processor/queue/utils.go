package queue

import "context"

func drainAncCloseChannel[T interface{}](ctx context.Context, channel chan T) error {
	for len(channel) > 0 {
		select {
		case <-channel:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	close(channel)
	return nil
}
