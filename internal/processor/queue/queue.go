package queue

import (
	"context"
	"time"
)

const SingleRequestTimeout = 30 * time.Second

type Queue interface {
	Produce(ctx context.Context, payload []byte) error
	// GetConsumerChan is closed by the consumer loop when it stops.
	GetConsumerChan() <-chan []byte
	StartQueueConsumer()
	StartQueueProducer()
	CloseQueue(context.Context) error
}
