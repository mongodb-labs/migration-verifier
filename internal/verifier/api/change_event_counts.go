package api

import "github.com/rs/zerolog"

// ChangeEventCounts tallies cumulative change events seen by a change reader,
// across all generations, since the verifier first started.
type ChangeEventCounts struct {
	Insert  int
	Update  int
	Replace int
	Delete  int
}

var _ zerolog.LogObjectMarshaler = ChangeEventCounts{}

func (cec ChangeEventCounts) Total() int {
	return cec.Insert + cec.Update + cec.Replace + cec.Delete
}

func (cec ChangeEventCounts) MarshalZerologObject(e *zerolog.Event) {
	e.
		Int("insert", cec.Insert).
		Int("update", cec.Update).
		Int("replace", cec.Replace).
		Int("delete", cec.Delete)
}
