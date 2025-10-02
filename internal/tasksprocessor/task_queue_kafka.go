package tasksprocessor

import (
	"context"
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

func (p *TaskProcessorKafka) AddTask(task Task) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	record := &kgo.Record{
		Topic: "tasks",
		Value: task,
	}
	p.KafkaClient.Produce(ctx, task)
}
