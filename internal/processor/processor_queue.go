package processor

import (
	"context"
	"encoding/json"
	"time"
	"web-crawler/internal/domain/config"
	"web-crawler/internal/processor/queue"
	"web-crawler/internal/webcrawler/runstates"

	"go.uber.org/zap"
)

type QueueProcessor struct {
	logger          *zap.SugaredLogger
	tasksQueue      queue.Queue
	runQueue        queue.Queue
	runStateManager runstates.RunStateManager

	tasksConsumer chan *config.Task
	runsConsumer  chan *config.Run
}

func NewTaskProcessorKafka(logger *zap.SugaredLogger, tasksQueue queue.Queue, runQueue queue.Queue, runStateManager runstates.RunStateManager) *QueueProcessor {
	return &QueueProcessor{
		logger:          logger,
		tasksQueue:      tasksQueue,
		runQueue:        runQueue,
		runStateManager: runStateManager,

		tasksConsumer: make(chan *config.Task, 100),
		runsConsumer:  make(chan *config.Run, 100),
	}
}

func (p *QueueProcessor) SendTask(task *config.Task) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	p.logger.Infow("SendTask called", "url", task.URL, "runID", task.Run.ID, "depth", task.CurrentDepth)

	err := p.checkRunInfo(ctx, task)
	if err != nil {
		p.logger.Warnw("Failed to update run info", "task", task)
		return err
	}

	bytes, err := json.Marshal(task)
	if err != nil {
		p.logger.Warnw("Failed to marshal task to json", "task", task)
		return err
	}

	if err := p.runStateManager.IncrementActiveAndCurrentLinks(ctx, task.Run.ID); err != nil {
		p.logger.Warnw("Failed to increment counters in Redis", "runID", task.Run.ID, "error", err)
		return err
	}

	p.logger.Infow("Sending task to Kafka", "url", task.URL, "runID", task.Run.ID)
	p.tasksQueue.GetProducerChan() <- bytes
	return nil
}

func (p *QueueProcessor) checkRunInfo(ctx context.Context, task *config.Task) error {
	currentLinks, err := p.runStateManager.GetCurrentLinks(ctx, task.Run.ID)
	if err != nil {
		p.logger.Warnw("Failed to get current links from Redis", "runID", task.Run.ID, "error", err)
		return err
	}

	if currentLinks >= int64(task.Run.MaxLinks) || task.CurrentDepth >= task.Run.MaxDepth {
		p.logger.Warnw("Task limit for links or depth is reached", "RunID", task.Run.ID, "currentLinks", currentLinks, "maxLinks", task.Run.MaxLinks)
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
	go p.tasksQueue.StartQueueConsumer()
	go p.tasksQueue.StartQueueProducer()

	p.logger.Info("StartTaskConsumer started, waiting for tasks from Kafka...")
	for taskBytes := range p.tasksQueue.GetConsumerChan() {
		task := new(config.Task)

		if err := json.Unmarshal(taskBytes, task); err != nil {
			p.logger.Warnw("Failed to unmarshal task to json", "taskBytes", string(taskBytes), "error", err)
			continue
		}

		p.logger.Infow("Received task from Kafka", "url", task.URL, "runID", task.Run.ID, "depth", task.CurrentDepth)
		p.tasksConsumer <- task
	}
}

func (p *QueueProcessor) GetTasksChan() <-chan *config.Task {
	return p.tasksConsumer
}

func (p *QueueProcessor) StartRunConsumer() {
	go p.runQueue.StartQueueConsumer()
	go p.runQueue.StartQueueProducer()

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
	//go func() {
	//	run := config.Run{
	//		ID:           "test",
	//		UseCacheFlag: true,
	//		MaxDepth:     3,
	//		MaxLinks:     15,
	//		ExtraFlags:   nil,
	//		StartURL:     "https://example.com/",
	//	}
	//
	//	p.runsConsumer <- &run
	//}()
	return p.runsConsumer
}
