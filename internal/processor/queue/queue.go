package queue

import "time"

const (
	ChannelBufferLimit = 50

	SingleRequestTimeout = 5 * time.Second
	queueTimeout         = 1 * time.Minute
	tickerTimeout        = 1 * time.Second
)

type Queue interface {
	GetProducerChan() chan []byte
	GetConsumerChan() chan []byte
	StartQueueConsumer()
	StartQueueProducer()
}
