package processor

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
	"web-crawler/internal/domain/config"
	"web-crawler/internal/processor/queue"
	"web-crawler/internal/webcrawler/runstates"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
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

// SendTask admits the task in Redis - one atomic step checks the run still holds
// its slot, drops a duplicate, enforces the link limit and counts the task - and
// only then produces it. A duplicate is not an error: the task is already queued.
func (p *QueueProcessor) SendTask(task *config.Task) error {
	if task.CurrentDepth >= task.Run.MaxDepth {
		return ErrRunLimitExceeded
	}

	task.EnsureID()

	payload, err := json.Marshal(task)
	if err != nil {
		return fmt.Errorf("marshalling task %s: %w", task.URL, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	admission, err := p.runStateManager.AdmitTask(ctx, task.Run.ID, task.ID, task.Run.MaxLinks)
	if err != nil {
		return err
	}

	switch admission {
	case runstates.Duplicate:
		return nil
	case runstates.LimitReached:
		return ErrRunLimitExceeded
	case runstates.RunInactive:
		return ErrRunNotActive
	}

	if err := p.tasksQueue.Produce(ctx, payload); err != nil {
		// Counted but never queued: undo it, or the run can never reach zero, and a
		// later retry would be rejected as a duplicate.
		if errUndo := p.runStateManager.UnadmitTask(context.Background(), task.Run.ID, task.ID); errUndo != nil {
			p.logger.Errorw("Failed to undo the admission of an unsent task", "runID", task.Run.ID, "error", errUndo)
		}

		return fmt.Errorf("queueing task %s: %w", task.URL, err)
	}

	p.logger.Debugw("Task queued", "url", task.URL, "runID", task.Run.ID, "depth", task.CurrentDepth)

	return nil
}

func (p *QueueProcessor) QueueRun(run *config.Run) {
	payload, err := json.Marshal(run)
	if err != nil {
		p.logger.Warnw("Failed to marshal run to json", "runID", run.ID, "error", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), queue.SingleRequestTimeout)
	defer cancel()

	if err := p.runQueue.Produce(ctx, payload); err != nil {
		p.logger.Errorw("Failed to re-queue run", "runID", run.ID, "error", err)
	}
}

// StartTaskConsumer hands tasks to the crawl workers with the message's ack
// attached. A message that is not a valid task is acked at once: left unacked it
// would hold back the commit point of its partition forever.
func (p *QueueProcessor) StartTaskConsumer() {
	go p.tasksQueue.StartQueueConsumer()
	go p.tasksQueue.StartQueueProducer()

	defer close(p.tasksConsumer)

	p.logger.Info("StartTaskConsumer started, waiting for tasks from Kafka...")

	for msg := range p.tasksQueue.GetConsumerChan() {
		task := new(config.Task)
		if err := json.Unmarshal(msg.Value, task); err != nil || task.Run == nil || task.Run.ID == "" || task.URL == "" {
			p.logger.Warnw("Skipping malformed task", "payload", string(msg.Value), "error", err)
			msg.Ack()

			continue
		}

		task.SetAck(msg.Ack)
		p.tasksConsumer <- task
	}
}

func (p *QueueProcessor) GetTasksChan() <-chan *config.Task {
	return p.tasksConsumer
}

func (p *QueueProcessor) StartRunConsumer() {
	go p.runQueue.StartQueueConsumer()
	go p.runQueue.StartQueueProducer()

	defer close(p.runsConsumer)

	for msg := range p.runQueue.GetConsumerChan() {
		run := new(config.Run)
		if err := json.Unmarshal(msg.Value, run); err != nil || run.ID == "" || run.StartURL == "" {
			p.logger.Warnw("Skipping malformed run", "payload", string(msg.Value), "error", err)
			msg.Ack()

			continue
		}

		run.SetAck(msg.Ack)
		p.runsConsumer <- run
	}
}

func (p *QueueProcessor) StopProcessor(ctx context.Context) error {
	eg := &errgroup.Group{}
	eg.Go(func() error {
		return p.runQueue.CloseQueue(ctx)
	})

	eg.Go(func() error {
		return p.tasksQueue.CloseQueue(ctx)
	})

	// runsConsumer and tasksConsumer are closed by the loops that send on them,
	// once the queues above stop feeding them.
	return eg.Wait()
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
