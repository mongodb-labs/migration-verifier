package api

import "github.com/rs/zerolog"

// ChangeEventCounts tallies cumulative change events seen by a change reader,
// across all generations, since the verifier first started.
type ChangeEventCounts struct {
	Insert  uint64
	Update  uint64
	Replace uint64
	Delete  uint64
}

var _ zerolog.LogObjectMarshaler = ChangeEventCounts{}

func (cec ChangeEventCounts) Total() uint64 {
	return cec.Insert + cec.Update + cec.Replace + cec.Delete
}

func (cec ChangeEventCounts) MarshalZerologObject(e *zerolog.Event) {
	e.
		Uint64("insert", cec.Insert).
		Uint64("update", cec.Update).
		Uint64("replace", cec.Replace).
		Uint64("delete", cec.Delete)
}
