package api

import "github.com/rs/zerolog"

// ChangeEventCounts tallies cumulative change events seen by a change reader,
// across all generations, since the verifier first started.
type ChangeEventCounts struct {
	// NB:  These are int64 because they get serialized to extJSON.
	// The API implementation’s Go types are unfortunately inconsistent.
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
	e.
		Int64("insert", cec.Insert).
		Int64("update", cec.Update).
		Int64("replace", cec.Replace).
		Int64("delete", cec.Delete)
}
