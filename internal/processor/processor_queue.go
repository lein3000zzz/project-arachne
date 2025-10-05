package processor

import (
	"encoding/json"
	"time"
	"web-crawler/internal/config"
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

func (p *QueueProcessor) SendTask(task *config.Task) error {
	bytes, err := json.Marshal(task)
	if err != nil {
		p.logger.Warnw("Failed to marshal task to json", "task", task)
		return err
	}

	err = p.updateRunInfo(task)
	if err != nil {
		p.logger.Warnw("Failed to update run info", "task", task)
		return err
	}

	p.tasksQueue.GetProducerChan() <- bytes
	return nil
}

func (p *QueueProcessor) updateRunInfo(task *config.Task) error {
	task.Run.Lock()
	defer task.Run.Unlock()

	if task.Run.CurrentLinks >= task.Run.MaxLinks || task.CurrentDepth >= task.Run.MaxDepth {
		p.logger.Warnw("Task limit for links or depth is reached", "RunID", task.Run.ID)
		return ErrRunLimitExceeded
	}

	task.Run.ActiveTasks++
	task.Run.CurrentLinks++
	return nil
}

func (p *QueueProcessor) GetRun() (*config.Run, error) {
	select {
	case runBytes := <-p.runQueue.GetConsumerChan():
		run := new(config.Run)

		if err := json.Unmarshal(runBytes, run); err != nil {
			p.logger.Warnw("Failed to unmarshal run from kafka", "record", runBytes, "err", err)
			return nil, err
		}

		return run, nil
	case <-time.After(queue.SingleRequestTimeout):
		return nil, ErrNoRuns
	}
}

func (p *QueueProcessor) QueueRun(run *config.Run) {
	bytes, err := json.Marshal(run)
	if err != nil {
		p.logger.Warnw("Failed to marshal task to json", "run", run)
		return
	}

	p.runQueue.GetProducerChan() <- bytes
}

func (p *QueueProcessor) GetTask() (*config.Task, error) {
	select {
	case taskBytes := <-p.tasksQueue.GetConsumerChan():
		task := new(config.Task)

		if err := json.Unmarshal(taskBytes, task); err != nil {
			p.logger.Warnw("Failed to unmarshal task from kafka", "record", taskBytes, "err", err)
			return nil, err
		}

		return task, nil
	case <-time.After(queue.SingleRequestTimeout):
		return nil, ErrNoTasks
	}
}
