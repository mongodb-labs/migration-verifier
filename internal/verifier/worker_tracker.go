package verifier

import (
	"fmt"
	"maps"
	"time"

	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/10gen/migration-verifier/msync"
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
	TaskID     any
	StartTime  time.Time
	TaskType   tasks.Type
	Namespace  string
	TaskStatus taskStatus
}

type taskStatus interface {
	isTaskStatus()
}

type DocumentTaskStatus struct {
	SrcDocCount  types.DocumentCount
	SrcByteCount types.ByteCount
}

func (DocumentTaskStatus) isTaskStatus() {}

type NamespaceTaskStatus struct {
	PartitionsCount int
}

func (NamespaceTaskStatus) isTaskStatus() {}

// NewWorkerTracker creates and returns a WorkerTracker.
func NewWorkerTracker(workersCount int) *WorkerTracker {
	wsmap := WorkerStatusMap{}
	for i := range workersCount {
		wsmap[i] = WorkerStatus{}
	}
	return &WorkerTracker{
		guard: msync.NewDataGuard(wsmap),
	}
}

// Set updates the worker’s state in the WorkerTracker.
func (wt *WorkerTracker) Set(workerNum int, task tasks.Task) {
	wt.guard.Store(func(m WorkerStatusMap) WorkerStatusMap {
		newStatus := WorkerStatus{
			TaskID:    task.PrimaryKey,
			TaskType:  task.Type,
			Namespace: task.QueryFilter.Namespace,
			StartTime: time.Now(),
		}

		switch task.Type {
		case tasks.VerifyDocuments:
			newStatus.TaskStatus = DocumentTaskStatus{}
		case tasks.VerifyCollection:
			newStatus.TaskStatus = NamespaceTaskStatus{}
		default:
			panic("unknown task type")
		}

		m[workerNum] = newStatus

		return m
	})
}

func (wt *WorkerTracker) SetDocTaskSrcCounts(workerNum int, docs types.DocumentCount, bytes types.ByteCount) {
	wt.guard.Store(func(m WorkerStatusMap) WorkerStatusMap {
		status := m[workerNum]

		dTaskStatus, ok := status.TaskStatus.(DocumentTaskStatus)
		if !ok {
			panic(fmt.Sprintf("attempting to set document counts on %T", status.TaskStatus))
		}
		dTaskStatus.SrcDocCount = docs
		dTaskStatus.SrcByteCount = bytes
		status.TaskStatus = dTaskStatus

		m[workerNum] = status

		return m
	})
}

func (wt *WorkerTracker) SetNamespaceTaskPartitionsCount(workerNum int, partitions int) {
	wt.guard.Store(func(m WorkerStatusMap) WorkerStatusMap {
		status := m[workerNum]
		nsTaskStatus, ok := status.TaskStatus.(NamespaceTaskStatus)
		if !ok {
			panic(fmt.Sprintf("attempting to set namespace partitions count on %T", status.TaskStatus))
		}
		nsTaskStatus.PartitionsCount = partitions
		status.TaskStatus = nsTaskStatus
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
