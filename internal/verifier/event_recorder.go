package verifier

import (
	"github.com/mongodb-labs/migration-verifier/msync"
	"github.com/pkg/errors"
	"golang.org/x/exp/maps"
)

type eventRecorderMap = map[string]PerNamespaceStats

// EventRecorder maintains statistics on change events.
type EventRecorder struct {
	guard *msync.DataGuard[eventRecorderMap]
}

// PerNamespaceStats records a given namespace’s event totals by optype.
type PerNamespaceStats struct {
	Insert  int
	Update  int
	Replace int
	Delete  int
}

func (pns PerNamespaceStats) Total() int {
	return pns.Insert + pns.Update + pns.Replace + pns.Delete
}

// NewEventRecorder creates and returns a new EventRecorder.
func NewEventRecorder() *EventRecorder {
	return &EventRecorder{
		guard: msync.NewDataGuard(eventRecorderMap{}),
	}
}

func (er EventRecorder) Reset() {
	er.guard.Store(func(m eventRecorderMap) eventRecorderMap {
		return eventRecorderMap{}
	})
}

// AddEvent adds a ParsedEvent to the EventRecorder’s statistics.
func (er EventRecorder) AddEvent(changeEvent *ParsedEvent) error {
	// This shouldn’t happen, but just in case:
	if changeEvent.Ns == nil {
		return errors.Errorf("Change event lacks a namespace: %+v", changeEvent)
	}

	nsStr := changeEvent.Ns.DB + "." + changeEvent.Ns.Coll

	var err error

	er.guard.Store(func(m eventRecorderMap) eventRecorderMap {
		if _, exists := m[nsStr]; !exists {
			m[nsStr] = PerNamespaceStats{}
		}

		nsStats := m[nsStr]

		switch changeEvent.OpType {
		case "insert":
			nsStats.Insert++
		case "update":
			nsStats.Update++
		case "replace":
			nsStats.Replace++
		case "delete":
			nsStats.Delete++
		default:
			err = errors.Errorf("Event recorder received event with unknown optype: %+v", *changeEvent)
		}

		m[nsStr] = nsStats

		return m
	})

	return err
}

// Read returns a map of the tracked change events. The map
// indexes on namespace then event optype. Each namespace will
// have `insert`, `update`
func (er EventRecorder) Read() eventRecorderMap {
	var theCopy eventRecorderMap

	er.guard.Load(func(m eventRecorderMap) {
		theCopy = maps.Clone(m)
	})

	return theCopy
}
