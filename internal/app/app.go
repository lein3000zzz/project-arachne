package app

const (
	ConcurrentTasksWorkers = 20
	ConcurrentRunsWorkers  = 1
)

type App interface {
	Start()
}
