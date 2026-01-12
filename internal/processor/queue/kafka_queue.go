package queue

import (
	"context"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"go.uber.org/zap"
)

type KafkaConfig struct {
	Seeds         []string
	ConsumerGroup string
	Topic         string
	User          string
	Password      string
}

type KafkaQueue struct {
	logger       *zap.SugaredLogger
	kafkaClient  *kgo.Client
	consumerChan chan []byte
	producerChan chan []byte
}

func NewKafkaQueue(logger *zap.SugaredLogger, config *KafkaConfig) (*KafkaQueue, error) {
	client, err := kgo.NewClient(
		kgo.SeedBrokers(config.Seeds...),
		kgo.ConsumerGroup(config.ConsumerGroup),
		kgo.ConsumeTopics(config.Topic),
		kgo.DefaultProduceTopic(config.Topic),
		kgo.SASL(plain.Auth{
			User: config.User,
			Pass: config.Password,
		}.AsMechanism()),
		kgo.DisableAutoCommit(),
	)

	if err != nil {
		logger.Errorw("error creating kafka client", "error", err)
		return nil, err
	}

	return &KafkaQueue{
		logger:       logger,
		kafkaClient:  client,
		consumerChan: make(chan []byte, ChannelBufferLimit),
		producerChan: make(chan []byte, ChannelBufferLimit),
	}, nil
}

func (q *KafkaQueue) GetProducerChan() chan<- []byte {
	return q.producerChan
}

func (q *KafkaQueue) GetConsumerChan() <-chan []byte {
	return q.consumerChan
}

func (q *KafkaQueue) StartQueueConsumer() {
	for {
		fetches := q.getFetches()
		iter := fetches.RecordIter()

		var recordsToCommit []*kgo.Record

		q.logger.Infow("working with some fetches", "fetches", fetches, "iter.Done()", iter.Done())

		for !iter.Done() {
			record := iter.Next()

			if record == nil {
				continue
			}

			q.processConsumedRecord(record)

			recordsToCommit = append(recordsToCommit, record)
		}

		if len(recordsToCommit) > 0 {
			q.commitRecords(recordsToCommit...)
		}
	}
}

// TODO сейчас те задачки, которые долго висят в очереди, просто пропускаются, то есть коммитятся как выполненные,
// даже когда это не так, пока что так и нужно
// TODO potential improvement: возвращать bool, чтобы в консьюмере коммитить только то, что было отправлено в канал,
// но не коммитить то, что ушло в таймаут
func (q *KafkaQueue) processConsumedRecord(record *kgo.Record) {
	ctx, cancel := context.WithTimeout(context.Background(), queueTimeout)
	defer cancel()

	select {
	case q.consumerChan <- record.Value:
		q.logger.Infow("sent record into the consumerChan", "record", record)
	case <-ctx.Done():
		q.logger.Warnw("Dropping record due to slow consumer or full channel", "record", record)
	}
}

func (q *KafkaQueue) getFetches() kgo.Fetches {
	ctx := context.Background()

	return q.kafkaClient.PollFetches(ctx)
}

func (q *KafkaQueue) commitRecords(records ...*kgo.Record) {
	if len(records) == 0 {
		q.logger.Warnw("No records to commit", "records", records)
		return
	}

	commitCtx, commitCancel := context.WithTimeout(context.Background(), SingleRequestTimeout)
	defer commitCancel()

	err := q.kafkaClient.CommitRecords(commitCtx, records...)
	if err != nil {
		q.logger.Warnw("Failed to commit task records in kafka", "records", records)
	}
}

func (q *KafkaQueue) StartQueueProducer() {
	items := make([][]byte, 0, ChannelBufferLimit)
	flushTicker := time.NewTicker(tickerTimeout)
	defer flushTicker.Stop()

	for {
		select {
		case item := <-q.producerChan:
			items = append(items, item)
			q.logger.Debugw("Received item in the producerChan", "item", item)

			if len(items) >= ChannelBufferLimit {
				q.flushItems(&items)
			}
		case <-flushTicker.C:
			q.flushItems(&items)
		}
	}
}

func (q *KafkaQueue) flushItems(items *[][]byte) {
	if len(*items) == 0 {
		return
	}
	q.logger.Debugw("Producing items", "items", *items)

	q.sendToKafka(*items)
	*items = (*items)[:0]
}

func (q *KafkaQueue) drainLoop(capacity int) [][]byte {
	drainedItems := make([][]byte, 0, capacity)

	for {
		select {
		case item := <-q.producerChan:
			drainedItems = append(drainedItems, item)
			q.logger.Infow("Received item in the procuderChan", "item", item)

			if len(drainedItems) >= capacity {
				return drainedItems
			}
		default:
			return drainedItems
		}
	}
}

func (q *KafkaQueue) sendToKafka(items [][]byte) {
	records := make([]*kgo.Record, 0, len(items))

	for _, taskBytes := range items {
		record := &kgo.Record{
			Value: taskBytes,
		}
		records = append(records, record)
	}

	q.produceRecords(records)
}

func (q *KafkaQueue) produceRecords(records []*kgo.Record) {
	ctx, cancel := context.WithTimeout(context.Background(), SingleRequestTimeout)
	defer cancel()

	var wg sync.WaitGroup

	for _, record := range records {
		wg.Add(1)
		q.kafkaClient.Produce(ctx, record, func(_ *kgo.Record, err error) {
			defer wg.Done()
			if err != nil {
				q.logger.Warnw("Failed to produce taskBytes record in kafka", "record", record, "err", err)
			}
		})
	}

	wg.Wait()

	q.logger.Infow("Produced items", "items", records)
}

func (q *KafkaQueue) CloseQueue() {
	drainAncCloseChannel(q.consumerChan)
	drainAncCloseChannel(q.producerChan)

	q.kafkaClient.Close()
}
