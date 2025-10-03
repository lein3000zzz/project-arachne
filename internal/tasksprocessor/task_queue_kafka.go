package tasksprocessor

import (
	"context"
	"encoding/json"
	"sync"
	"time"
	"web-crawler/internal/utils"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.uber.org/zap"
)

// TODO

type TaskProcessorKafka struct {
	logger            *zap.SugaredLogger
	kafkaClient       *kgo.Client
	tasksConsumerChan chan *Task
	tasksProducerChan chan *Task
}

func NewTaskProcessorKafka(logger *zap.SugaredLogger, seeds []string) *TaskProcessorKafka {
	client, err := kgo.NewClient(
		kgo.SeedBrokers(seeds...),
		kgo.ConsumerGroup("arachne"),
		kgo.ConsumeTopics("tasks"),
	)

	if err != nil {
		logger.Fatal("Error connecting to kafka:", err)
	}

	return &TaskProcessorKafka{
		logger:            logger,
		kafkaClient:       client,
		tasksConsumerChan: make(chan *Task, 50),
		tasksProducerChan: make(chan *Task, 50),
	}
}

func (p *TaskProcessorKafka) SendTask(task *Task) {
	p.tasksProducerChan <- task
}

func (p *TaskProcessorKafka) GetTask() (*Task, error) {
	select {
	case task := <-p.tasksConsumerChan:
		return task, nil
	case <-time.After(singleRequestTimeout):
		return nil, ErrNoTasks
	}
}

func (p *TaskProcessorKafka) StartTaskProducer() {
	tasks := make([][]byte, 0, 50)
	flushTicker := time.NewTicker(tickerTimeout)
	defer flushTicker.Stop()

	for {
		select {
		case task := <-p.tasksProducerChan:
			bytes, err := json.Marshal(task)
			if err != nil {
				p.logger.Warnw("Failed to marshal task to json", "task", task)
				continue
			}

			tasks = append(tasks, bytes)
			if len(tasks) >= 50 {
				p.logger.Infow("Producing tasks", "tasks", tasks)
				p.sendTasks(tasks)
				tasks = make([][]byte, 0, 50)
			}
		case <-flushTicker.C:
			if len(tasks) > 0 {
				p.logger.Infow("Producing tasks", "tasks", tasks)
				p.sendTasks(tasks)
				tasks = make([][]byte, 0, 50)
			}
		}
	}
}

func (p *TaskProcessorKafka) sendTasks(tasks [][]byte) {
	records := make([]*kgo.Record, 0, len(tasks))

	for _, taskBytes := range tasks {
		record := &kgo.Record{
			Topic: "tasks",
			Value: taskBytes,
		}
		records = append(records, record)
	}

	var wg sync.WaitGroup

	for _, record := range records {
		wg.Add(1)
		p.produceRecord(record, &wg)
	}
	wg.Wait()
}

func (p *TaskProcessorKafka) produceRecord(record *kgo.Record, wg *sync.WaitGroup) {
	ctx, cancel := context.WithTimeout(context.Background(), singleRequestTimeout)
	defer cancel()

	p.kafkaClient.Produce(ctx, record, func(_ *kgo.Record, err error) {
		wg.Done()
		if err != nil {
			p.logger.Warnw("Failed to produce taskBytes record in kafka", "record", record)
		}
	})
}

func (p *TaskProcessorKafka) StartTaskConsumer() {
	timer := time.NewTimer(queueTimeout)
	utils.DrainTimer(timer)

	for {
		fetches := p.getFetches()
		iter := fetches.RecordIter()

		var recordsToCommit []*kgo.Record

		for !iter.Done() {
			record := iter.Next()

			if record == nil {
				continue
			}

			task := new(Task)
			if err := json.Unmarshal(record.Value, task); err != nil {
				p.logger.Warnw("Failed to unmarshal task from kafka", "record", record, "err", err)
				continue
			}

			timer.Reset(queueTimeout)

			select {
			case p.tasksConsumerChan <- task:
				utils.DrainTimer(timer)
			case <-time.After(queueTimeout):
				p.logger.Warnw("Dropping task due to slow consumer or full channel", "task", task)
			}

			recordsToCommit = append(recordsToCommit, record)
		}

		if len(recordsToCommit) > 0 {
			p.commitRecords(recordsToCommit...)
		}
	}
}

func (p *TaskProcessorKafka) getFetches() kgo.Fetches {
	ctx, cancel := context.WithTimeout(context.Background(), singleRequestTimeout)
	defer cancel()

	return p.kafkaClient.PollFetches(ctx)
}

func (p *TaskProcessorKafka) commitRecords(records ...*kgo.Record) {
	if len(records) == 0 {
		p.logger.Warnw("No records to commit", "records", records)
		return
	}

	commitCtx, commitCancel := context.WithTimeout(context.Background(), singleRequestTimeout)
	defer commitCancel()

	err := p.kafkaClient.CommitRecords(commitCtx, records...)
	if err != nil {
		p.logger.Warnw("Failed to commit task records in kafka", "records", records)
	}
}
