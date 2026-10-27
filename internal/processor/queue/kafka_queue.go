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

	ChannelBuffer  int
	RequestTimeout time.Duration
	FlushInterval  time.Duration
}

type KafkaQueue struct {
	logger       *zap.SugaredLogger
	kafkaClient  *kgo.Client
	consumerChan chan Message
	producerChan chan []byte
	cfg          *KafkaConfig
	offsets      *offsetTracker

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
	stop, cancel := context.WithCancel(context.Background())

	q := &KafkaQueue{
		logger:       logger,
		consumerChan: make(chan Message, config.ChannelBuffer),
		producerChan: make(chan []byte, config.ChannelBuffer),
		cfg:          config,
		offsets:      newOffsetTracker(),
		stop:         stop,
		cancelStop:   cancel,
	}

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
		kgo.WithHooks(kotel.NewKotel().Hooks()),
		// Offsets are committed only for processed records, by the committer loop.
		// franz-go's autocommit would commit whatever was polled.
		kgo.DisableAutoCommit(),
		kgo.OnPartitionsRevoked(q.onRevoked),
		kgo.OnPartitionsLost(q.onLost),
	)
	if err != nil {
		cancel()
		logger.Errorw("error creating kafka client", "error", err)

		return nil, err
	}

	q.kafkaClient = client
	q.loops.Add(3)

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

func (q *KafkaQueue) GetConsumerChan() <-chan Message {
	return q.consumerChan
}

// StartQueueConsumer delivers records in partition order and commits a record's
// offset only once it and everything before it were acked. A record that is never
// acked - its consumer died, the queue stopped first - is redelivered.
func (q *KafkaQueue) StartQueueConsumer() {
	go q.runCommitter()

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

		for iter := fetches.RecordIter(); !iter.Done(); {
			record := iter.Next()
			q.offsets.track(record)

			select {
			case q.consumerChan <- Message{Value: record.Value, ack: func() { q.offsets.done(record) }}:
			case <-q.stop.Done():
				return
			}
		}
	}
}

func (q *KafkaQueue) runCommitter() {
	defer q.loops.Done()

	ticker := time.NewTicker(q.cfg.FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			q.commit(q.kafkaClient, q.offsets.take())
		case <-q.stop.Done():
			q.commit(q.kafkaClient, q.offsets.take())
			return
		}
	}
}

func (q *KafkaQueue) commit(client *kgo.Client, records []*kgo.Record) {
	if len(records) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), q.cfg.RequestTimeout)
	defer cancel()

	if err := client.CommitRecords(ctx, records...); err != nil {
		q.offsets.restore(records)
		q.logger.Warnw("Failed to commit offsets, will retry", "records", len(records), "err", err)
	}
}

// onRevoked commits what is already done in partitions moving to another
// consumer. Records still in flight from them are redelivered there.
func (q *KafkaQueue) onRevoked(_ context.Context, client *kgo.Client, revoked map[string][]int32) {
	q.commit(client, q.offsets.release(revoked))
}

// onLost cannot commit: the partitions already belong to someone else.
func (q *KafkaQueue) onLost(_ context.Context, _ *kgo.Client, lost map[string][]int32) {
	q.offsets.release(lost)
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
