package verifier

import (
	"time"

	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/10gen/migration-verifier/msync"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
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
	TaskID    bson.ObjectID
	StartTime time.Time
	TaskType  tasks.Type
	Namespace string

	PartitionCount      int
	IdealPartitionCount int

	// For document-comparison tasks:
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

func (wt *WorkerTracker) SetPartitionCounts(taskID bson.ObjectID, soFar int, target int) {
	wt.guard.Store(func(m WorkerStatusMap) WorkerStatusMap {
		for workerNum, status := range m {
			if taskID != status.TaskID {
				continue
			}

			lo.Assertf(
				status.TaskType == tasks.VerifyCollection,
				"task type must be collection metadata (found %#q)",
				status.TaskType,
			)

			status.PartitionCount = soFar
			status.IdealPartitionCount = target
			m[workerNum] = status

			break
		}

		return m
	})
}

func (wt *WorkerTracker) SetSrcCounts(workerNum int, docs types.DocumentCount, bytes types.ByteCount) {
	wt.guard.Store(func(m WorkerStatusMap) WorkerStatusMap {
		status := m[workerNum]

		lo.Assertf(
			status.TaskType == tasks.VerifyDocuments,
			"task type must be document comparison (found %#q)",
			status.TaskType,
		)

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
