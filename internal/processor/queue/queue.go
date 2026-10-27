package queue

import (
	"context"
	"time"
)

const SingleRequestTimeout = 30 * time.Second

type Queue interface {
	Produce(ctx context.Context, payload []byte) error
	// GetConsumerChan is closed by the consumer loop when it stops.
	GetConsumerChan() <-chan Message
	StartQueueConsumer()
	StartQueueProducer()
	CloseQueue(context.Context) error
}

type Message struct {
	Value []byte
	ack   func()
}

func NewMessage(value []byte, ack func()) Message {
	return Message{Value: value, ack: ack}
}

// Ack marks the message processed. Its offset is committed once every earlier
// message in its partition is acked as well; until then a restart or rebalance
// redelivers it.
func (m Message) Ack() {
	if m.ack != nil {
		m.ack()
	}
}
