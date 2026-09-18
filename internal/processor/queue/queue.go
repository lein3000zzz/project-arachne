package queue

import (
	"context"
	"time"
)

const SingleRequestTimeout = 30 * time.Second

type Queue interface {
	GetProducerChan() chan<- []byte
	GetConsumerChan() <-chan []byte
	StartQueueConsumer()
	StartQueueProducer()
	CloseQueue(context.Context) error
}
