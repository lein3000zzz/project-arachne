package tasksprocessor

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.uber.org/zap"
)

// TODO

type TaskProcessorKafka struct {
	Logger      *zap.SugaredLogger
	KafkaClient *kgo.Client
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
		Logger:      logger,
		KafkaClient: client,
	}
}

func (p *TaskProcessorKafka) AddTask(task *Task) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	bytes, err := json.Marshal(task)
	if err != nil {
		p.Logger.Warnw("Failed to marshal task to json", "task", task)
		return
	}

	record := &kgo.Record{
		Topic: "tasks",
		Value: bytes,
	}

	var wg sync.WaitGroup
	wg.Add(1)
	p.KafkaClient.Produce(ctx, record, func(_ *kgo.Record, err error) {
		if err != nil {
			p.Logger.Warnw("Failed to produce task record in kafka", "record", record)
		}
	})
	wg.Wait()
}

func (p *TaskProcessorKafka) GetTask() (*Task, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	commitCtx, commitCancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer commitCancel()

	fetches := p.KafkaClient.PollFetches(ctx)
	iter := fetches.RecordIter()

	var record *kgo.Record
	for !iter.Done() {
		record = iter.Next()

		if record == nil {
			continue
		}

		var task Task
		if err := json.Unmarshal(record.Value, &task); err != nil {
			p.Logger.Warnw("Failed to unmarshal task from kafka", "record", record)
			continue
		}

		//p.KafkaClient.CommitOffsets(commitCtx, record)

		return &task, nil
	}

	return nil, ErrNoTasks
}
