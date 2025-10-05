package queue

import (
	"context"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.uber.org/zap"
)

type KafkaQueue struct {
	logger       *zap.SugaredLogger
	KafkaClient  *kgo.Client
	topic        string
	consumerChan chan []byte
	producerChan chan []byte
}

func NewKafkaQueue(logger *zap.SugaredLogger, seeds []string, consumerGroup, topic string) (*KafkaQueue, error) {
	client, err := kgo.NewClient(
		kgo.SeedBrokers(seeds...),
		kgo.ConsumerGroup(consumerGroup),
		kgo.ConsumeTopics(topic),
	)

	if err != nil {
		return nil, err
	}

	return &KafkaQueue{
		logger:       logger,
		KafkaClient:  client,
		topic:        topic,
		consumerChan: make(chan []byte, ChannelBufferLimit),
		producerChan: make(chan []byte, ChannelBufferLimit),
	}, nil
}

func (q *KafkaQueue) GetProducerChan() chan []byte {
	return q.producerChan
}

func (q *KafkaQueue) GetConsumerChan() chan []byte {
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

	return q.KafkaClient.PollFetches(ctx)
}

func (q *KafkaQueue) commitRecords(records ...*kgo.Record) {
	if len(records) == 0 {
		q.logger.Warnw("No records to commit", "records", records)
		return
	}

	commitCtx, commitCancel := context.WithTimeout(context.Background(), SingleRequestTimeout)
	defer commitCancel()

	err := q.KafkaClient.CommitRecords(commitCtx, records...)
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
			q.logger.Infow("Received item in the procuderChan", "item", item)
			if len(items) >= ChannelBufferLimit {
				q.logger.Infow("Producing items", "items", items)
				q.sendToKafka(items)
				items = make([][]byte, 0, ChannelBufferLimit)
			}
		case <-flushTicker.C:
			remainingCap := cap(items) - len(items)
			items = append(items, q.drainLoop(remainingCap)...)

			if len(items) > 0 {
				q.logger.Infow("Producing items", "items", items)
				q.sendToKafka(items)
				items = make([][]byte, 0, ChannelBufferLimit)
			}
		}
	}
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
			Topic: q.topic,
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
		q.KafkaClient.Produce(ctx, record, func(_ *kgo.Record, err error) {
			defer wg.Done()
			if err != nil {
				q.logger.Warnw("Failed to produce taskBytes record in kafka", "record", record, "err", err)
			}
		})
	}

	wg.Wait()

	q.logger.Infow("Produced items", "items", records)
}
