package processor

import (
	"encoding/json"
	"time"
	"web-crawler/internal/domain/config"
	"web-crawler/internal/processor/queue"

	"go.uber.org/zap"
)

type QueueProcessor struct {
	logger     *zap.SugaredLogger
	tasksQueue queue.Queue
	runQueue   queue.Queue

	tasksConsumer chan *config.Task
	runsConsumer  chan *config.Run
}

func NewTaskProcessorKafka(logger *zap.SugaredLogger, tasksQueue queue.Queue, runQueue queue.Queue) *QueueProcessor {
	return &QueueProcessor{
		logger:     logger,
		tasksQueue: tasksQueue,
		runQueue:   runQueue,

		tasksConsumer: make(chan *config.Task, 100),
		runsConsumer:  make(chan *config.Run, 100),
	}
}

func (p *QueueProcessor) SendTask(task *config.Task) error {
	err := p.checkRunInfo(task)
	if err != nil {
		p.logger.Warnw("Failed to update run info", "task", task)
		return err
	}

	bytes, err := json.Marshal(task)
	if err != nil {
		p.logger.Warnw("Failed to marshal task to json", "task", task)
		return err
	}

	task.Run.IncrementActiveAndCurrentWithMutex()

	p.tasksQueue.GetProducerChan() <- bytes
	return nil
}

func (p *QueueProcessor) checkRunInfo(task *config.Task) error {
	task.Run.RLock()
	defer task.Run.RUnlock()

	if task.Run.CurrentLinks >= task.Run.MaxLinks || task.CurrentDepth >= task.Run.MaxDepth {
		p.logger.Warnw("Task limit for links or depth is reached", "RunID", task.Run.ID)
		return ErrRunLimitExceeded
	}

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

func (p *QueueProcessor) StartTaskConsumer() {
	for taskBytes := range p.tasksQueue.GetConsumerChan() {
		task := new(config.Task)

		if err := json.Unmarshal(taskBytes, task); err != nil {
			p.logger.Warnw("Failed to unmarshal task to json", "task", task)
			continue
		}

		p.tasksConsumer <- task
	}
}

func (p *QueueProcessor) GetTasksChan() <-chan *config.Task {
	return p.tasksConsumer
}

func (p *QueueProcessor) StartRunConsumer() {
	for runBytes := range p.runQueue.GetConsumerChan() {
		run := new(config.Run)

		if err := json.Unmarshal(runBytes, run); err != nil {
			p.logger.Warnw("Failed to unmarshal run from kafka", "run", run)
			continue
		}

		p.runsConsumer <- run
	}
}

func (p *QueueProcessor) GetRunsChan() <-chan *config.Run {
	return p.runsConsumer
}
