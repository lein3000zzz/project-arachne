package queue

import (
	"context"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/plugin/kotel"
	"go.uber.org/zap"
)

type KafkaConfig struct {
	Seeds         []string
	ConsumerGroup string
	Topic         string
	User          string
	Password      string

	ChannelBuffer   int
	RequestTimeout  time.Duration
	ConsumerTimeout time.Duration
	FlushInterval   time.Duration
}

type KafkaQueue struct {
	logger       *zap.SugaredLogger
	kafkaClient  *kgo.Client
	consumerChan chan []byte
	producerChan chan []byte
	cfg          *KafkaConfig

	stop       context.Context
	cancelStop context.CancelFunc
	loops      sync.WaitGroup

	// Guards the producer channel across shutdown: Produce holds it shared while
	// sending, the producer loop takes it exclusively before its final drain, so
	// nothing can land in the channel after that drain.
	sendMu sync.RWMutex
	sealed bool
}

func NewKafkaQueue(logger *zap.SugaredLogger, config *KafkaConfig) (*KafkaQueue, error) {
	kotelClient := kotel.NewKotel()

	client, err := kgo.NewClient(
		kgo.SeedBrokers(config.Seeds...),
		kgo.ConsumerGroup(config.ConsumerGroup),
		kgo.ConsumeTopics(config.Topic),
		kgo.DefaultProduceTopic(config.Topic),
		kgo.AllowAutoTopicCreation(),
		kgo.ProduceRequestTimeout(config.RequestTimeout),
		kgo.RecordDeliveryTimeout(60*time.Second),
		kgo.SASL(plain.Auth{
			User: config.User,
			Pass: config.Password,
		}.AsMechanism()),
		kgo.WithHooks(kotelClient.Hooks()),
	)

	if err != nil {
		logger.Errorw("error creating kafka client", "error", err)
		return nil, err
	}

	stop, cancel := context.WithCancel(context.Background())

	q := &KafkaQueue{
		logger:       logger,
		kafkaClient:  client,
		consumerChan: make(chan []byte, config.ChannelBuffer),
		producerChan: make(chan []byte, config.ChannelBuffer),
		cfg:          config,
		stop:         stop,
		cancelStop:   cancel,
	}
	q.loops.Add(2)

	return q, nil
}

func (q *KafkaQueue) Produce(ctx context.Context, payload []byte) error {
	q.sendMu.RLock()
	defer q.sendMu.RUnlock()

	if q.sealed {
		return ErrQueueClosed
	}

	select {
	case q.producerChan <- payload:
		return nil
	case <-q.stop.Done():
		return ErrQueueClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (q *KafkaQueue) GetConsumerChan() <-chan []byte {
	return q.consumerChan
}

func (q *KafkaQueue) StartQueueConsumer() {
	defer q.loops.Done()
	defer close(q.consumerChan)

	for {
		fetches := q.kafkaClient.PollFetches(q.stop)
		if q.stop.Err() != nil {
			return
		}

		fetches.EachError(func(topic string, partition int32, err error) {
			q.logger.Warnw("Kafka fetch error", "topic", topic, "partition", partition, "err", err)
		})

		iter := fetches.RecordIter()
		recordsToCommit := make([]*kgo.Record, 0)

		for !iter.Done() {
			record := iter.Next()
			if record == nil {
				continue
			}

			if !q.processConsumedRecord(record) {
				q.commitRecords(recordsToCommit...)
				return
			}

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
func (q *KafkaQueue) processConsumedRecord(record *kgo.Record) bool {
	ctx, cancel := context.WithTimeout(q.stop, q.cfg.ConsumerTimeout)
	defer cancel()

	select {
	case q.consumerChan <- record.Value:
		q.logger.Debugw("sent record into the consumerChan", "offset", record.Offset)
		return true
	case <-ctx.Done():
		if q.stop.Err() != nil {
			return false
		}

		q.logger.Warnw("Dropping record due to slow consumer or full channel", "offset", record.Offset)

		return true
	}
}

func (q *KafkaQueue) commitRecords(records ...*kgo.Record) {
	if len(records) == 0 {
		q.logger.Warnw("No records to commit", "records", records)
		return
	}

	commitCtx, commitCancel := context.WithTimeout(context.Background(), q.cfg.RequestTimeout)
	defer commitCancel()

	err := q.kafkaClient.CommitRecords(commitCtx, records...)
	if err != nil {
		q.logger.Warnw("Failed to commit task records in kafka", "records", records)
	}
}

func (q *KafkaQueue) StartQueueProducer() {
	defer q.loops.Done()

	runProducer(q.stop.Done(), q.producerChan, q.cfg.ChannelBuffer, q.cfg.FlushInterval, q.seal, q.sendToKafka)
}

func (q *KafkaQueue) seal() {
	q.sendMu.Lock()
	q.sealed = true
	q.sendMu.Unlock()
}

// runProducer batches payloads and flushes them. On stop it flushes what is still
// buffered instead of dropping it: every queued task was already counted in its
// run's active-task counter, and a lost one keeps the run from ever completing.
func runProducer(stop <-chan struct{}, in <-chan []byte, batchSize int, interval time.Duration, seal func(), flush func([][]byte)) {
	items := make([][]byte, 0, batchSize)

	send := func() {
		if len(items) > 0 {
			flush(items)
			items = make([][]byte, 0, batchSize)
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case item := <-in:
			items = append(items, item)
			if len(items) >= batchSize {
				send()
			}
		case <-ticker.C:
			send()
		case <-stop:
			seal()

			for {
				select {
				case item := <-in:
					items = append(items, item)
					if len(items) >= batchSize {
						send()
					}
				default:
					send()
					return
				}
			}
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
	ctx, cancel := context.WithTimeout(context.Background(), q.cfg.RequestTimeout)
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

func (q *KafkaQueue) CloseQueue(ctx context.Context) error {
	q.cancelStop()

	finished := make(chan struct{})
	go func() {
		q.loops.Wait()
		close(finished)
	}()

	var err error

	select {
	case <-finished:
	case <-ctx.Done():
		err = ctx.Err()
	}

	q.kafkaClient.Close()

	return err
}
