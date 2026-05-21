package api

import (
	"github.com/rs/zerolog"
)

// ChangeEventCounts tallies cumulative change events seen by a change reader,
// across all generations, since the verifier first started.
type ChangeEventCounts struct {
	// NB:  These are int64 because they get serialized to extJSON.
	// The API implementation's Go types are unfortunately inconsistent.
	Insert  int64
	Update  int64
	Replace int64
	Delete  int64
}

var _ zerolog.LogObjectMarshaler = ChangeEventCounts{}

func (cec ChangeEventCounts) Total() int64 {
	return cec.Insert + cec.Update + cec.Replace + cec.Delete
}

func (cec ChangeEventCounts) MarshalZerologObject(e *zerolog.Event) {
	if cec.Insert > 0 {
		e.Int64("insert", cec.Insert)
	}
	if cec.Update > 0 {
		e.Int64("update", cec.Update)
	}
	if cec.Replace > 0 {
		e.Int64("replace", cec.Replace)
	}
	if cec.Delete > 0 {
		e.Int64("delete", cec.Delete)
	}
}
