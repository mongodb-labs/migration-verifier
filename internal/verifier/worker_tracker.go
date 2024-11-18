package verifier

import (
	"time"

	"github.com/10gen/migration-verifier/msync"
)

type WorkerTracker struct {
	guard *msync.DataGuard[WorkerStatusMap]
}

type WorkerStatusMap = map[int]WorkerStatus

type WorkerStatus struct {
	TaskID    any
	TaskType  verificationTaskType
	Namespace string
	StartTime time.Time
}

func NewWorkerTracker(workersCount int) *WorkerTracker {
	wsmap := WorkerStatusMap{}
	for i := 0; i < workersCount; i++ {
		wsmap[i] = WorkerStatus{}
	}
	return &WorkerTracker{
		guard: msync.NewDataGuard(wsmap),
	}
}

func (wt *WorkerTracker) Set(workerNum int, task VerificationTask) {
	wt.guard.Store(func(m WorkerStatusMap) WorkerStatusMap {
		m[workerNum] = WorkerStatus{
			TaskID:    task.PrimaryKey,
			TaskType:  task.Type,
			Namespace: task.QueryFilter.Namespace,
			StartTime: time.Now(),
		}

		return m
	})
}

func (wt *WorkerTracker) Unset(workerNum int) {
	wt.guard.Store(func(m WorkerStatusMap) WorkerStatusMap {
		m[workerNum] = WorkerStatus{}

		return m
	})
}

func (wt *WorkerTracker) Load() WorkerStatusMap {
	var wtmap WorkerStatusMap
	wt.guard.Load(func(m map[int]WorkerStatus) {
		wtmap = m
	})

	return wtmap
}
