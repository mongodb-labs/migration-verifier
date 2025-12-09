package agg

import "go.mongodb.org/mongo-driver/v2/bson"

// Slice represents `$slice`’s 2-argument form.
// See SliceFrom for that operator’s 3-argument form.
type Slice struct {
	Array any
	N     any
}

func (s Slice) MarshalBSON() ([]byte, error) {
	return bson.Marshal(bson.D{
		{"$slice", []any{s.Array, s.N}},
	})
}

type SliceFrom struct {
	Array    any
	Position any
	N        any
}

func (sf SliceFrom) MarshalBSON() ([]byte, error) {
	return bson.Marshal(bson.D{
		{"$slice", []any{sf.Array, sf.Position, sf.N}},
	})
}
