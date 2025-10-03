package processor

import (
	"encoding/json"
	"time"
	"web-crawler/internal/processor/queue"

	"go.uber.org/zap"
)

type QueueProcessor struct {
	logger     *zap.SugaredLogger
	tasksQueue queue.Queue
	runQueue   queue.Queue
}

func NewTaskProcessorKafka(logger *zap.SugaredLogger, tasksQueue queue.Queue, runQueue queue.Queue) *QueueProcessor {
	return &QueueProcessor{
		logger:     logger,
		tasksQueue: tasksQueue,
		runQueue:   runQueue,
	}
}

func (p *QueueProcessor) SendTask(task *Task) {
	bytes, err := json.Marshal(task)
	if err != nil {
		p.logger.Warnw("Failed to marshal task to json", "task", task)
		return
	}

	p.tasksQueue.GetProducerChan() <- bytes
}

func (p *QueueProcessor) GetTask() (*Task, error) {
	select {
	case taskBytes := <-p.tasksQueue.GetConsumerChan():
		task := new(Task)

		if err := json.Unmarshal(taskBytes, task); err != nil {
			p.logger.Warnw("Failed to unmarshal task from kafka", "record", taskBytes, "err", err)
			return nil, err
		}

		return task, nil
	case <-time.After(queue.SingleRequestTimeout):
		return nil, ErrNoTasks
	}
}
