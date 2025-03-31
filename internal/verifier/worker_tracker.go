package verifier

import (
	"time"

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
type WorkerStatusMap = map[int]any

// WorkerTaskStatus details the work that an individual worker thread
// is doing on a task.
type WorkerTaskStatus struct {
	TaskID    any
	TaskType  verificationTaskType
	Namespace string
	StartTime time.Time
}

// WorkerPendingStatus details an individual worker thread that’s
// querying for a new task.
type WorkerTaskQuerying struct {
	StartTime time.Time
}

// NewWorkerTracker creates and returns a WorkerTracker.
func NewWorkerTracker(workersCount int) *WorkerTracker {
	wsmap := WorkerStatusMap{}
	for i := 0; i < workersCount; i++ {
		wsmap[i] = WorkerTaskStatus{}
	}
	return &WorkerTracker{
		guard: msync.NewDataGuard(wsmap),
	}
}

// SetTask tells the WorkerTracker about a task that the worker is starting.
func (wt *WorkerTracker) SetTask(workerNum int, task VerificationTask) {
	wt.guard.Store(func(m WorkerStatusMap) WorkerStatusMap {
		m[workerNum] = WorkerTaskStatus{
			TaskID:    task.PrimaryKey,
			TaskType:  task.Type,
			Namespace: task.QueryFilter.Namespace,
			StartTime: time.Now(),
		}

		return m
	})
}

// SetTask tells the WorkerTracker about a task that the worker is starting.
func (wt *WorkerTracker) SetQuerying(workerNum int) {
	wt.guard.Store(func(m WorkerStatusMap) WorkerStatusMap {
		m[workerNum] = WorkerTaskQuerying{
			StartTime: time.Now(),
		}

		return m
	})
}

// Unset tells the WorkerTracker that the worker is now inactive.
func (wt *WorkerTracker) Unset(workerNum int) {
	wt.guard.Store(func(m WorkerStatusMap) WorkerStatusMap {
		m[workerNum] = nil

		return m
	})
}

// Load duplicates and returns the WorkerTracker’s internal
// state map.
func (wt *WorkerTracker) Load() WorkerStatusMap {
	var wtmap WorkerStatusMap
	wt.guard.Load(func(m map[int]any) {
		wtmap = maps.Clone(m)
	})

	return wtmap
}
