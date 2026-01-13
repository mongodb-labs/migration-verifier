package verifier

import (
	"time"

	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/10gen/migration-verifier/msync"
	"golang.org/x/exp/maps"
)

// WorkerTracker holds certain data points about each worker thread
// in a check generation. It is thread-safe.
type WorkerTracker struct {
	guard *msync.DataGuard[WorkerStatusMap]
}

// WorkerStatusMap represents the status of each worker,
// indexed by worker number (which start at 0).
type WorkerStatusMap = map[int]WorkerStatus

// WorkerStatus details the work that an individual worker thread
// is doing.
type WorkerStatus struct {
	TaskID       any
	StartTime    time.Time
	TaskType     tasks.Type
	Namespace    string
	SrcDocCount  types.DocumentCount
	SrcByteCount types.ByteCount
}

// NewWorkerTracker creates and returns a WorkerTracker.
func NewWorkerTracker(workersCount int) *WorkerTracker {
	wsmap := WorkerStatusMap{}
	for i := 0; i < workersCount; i++ {
		wsmap[i] = WorkerStatus{}
	}
	return &WorkerTracker{
		guard: msync.NewDataGuard(wsmap),
	}
}

// Set updates the worker’s state in the WorkerTracker.
func (wt *WorkerTracker) Set(workerNum int, task tasks.Task) {
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

func (wt *WorkerTracker) SetSrcCounts(workerNum int, docs types.DocumentCount, bytes types.ByteCount) {
	wt.guard.Store(func(m WorkerStatusMap) WorkerStatusMap {
		status := m[workerNum]
		status.SrcDocCount = docs
		status.SrcByteCount = bytes
		m[workerNum] = status

		return m
	})
}

// Unset tells the WorkerTracker that the worker is now inactive.
func (wt *WorkerTracker) Unset(workerNum int) {
	wt.guard.Store(func(m WorkerStatusMap) WorkerStatusMap {
		m[workerNum] = WorkerStatus{}

		return m
	})
}

// Load duplicates and returns the WorkerTracker’s internal
// state map.
func (wt *WorkerTracker) Load() WorkerStatusMap {
	var wtmap WorkerStatusMap
	wt.guard.Load(func(m map[int]WorkerStatus) {
		wtmap = maps.Clone(m)
	})

	return wtmap
}
