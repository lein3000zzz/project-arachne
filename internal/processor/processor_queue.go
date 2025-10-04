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

func (p *QueueProcessor) SendTask(task *Task) error {
	bytes, err := json.Marshal(task)
	if err != nil {
		p.logger.Warnw("Failed to marshal task to json", "task", task)
		return err
	}

	if p.IsRunLimitExceeded(task) {
		p.logger.Warnw("Task limit exceeded", "task", task)
		return ErrRunLimitExceeded
	}

	task.Run.IncrementLinks()

	p.tasksQueue.GetProducerChan() <- bytes
	return nil
}

func (p *QueueProcessor) IsRunLimitExceeded(task *Task) bool {
	task.Run.RLock()
	defer task.Run.RUnlock()

	if task.Run.currentLinks >= task.Run.MaxLinks || task.CurrentDepth >= task.Run.MaxDepth {
		p.logger.Warnw("Task limit for links or depth is reached", "RunID", task.Run.ID)
		return true
	}

	return false
}

func (p *QueueProcessor) GetRun() (*Run, error) {
	select {
	case runBytes := <-p.runQueue.GetConsumerChan():
		run := new(Run)

		if err := json.Unmarshal(runBytes, run); err != nil {
			p.logger.Warnw("Failed to unmarshal run from kafka", "record", runBytes, "err", err)
			return nil, err
		}

		return run, nil
	case <-time.After(queue.SingleRequestTimeout):
		return nil, ErrNoRuns
	}
}

func (p *QueueProcessor) QueueRun(run *Run) {
	bytes, err := json.Marshal(run)
	if err != nil {
		p.logger.Warnw("Failed to marshal task to json", "run", run)
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
