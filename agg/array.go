package agg

import (
	"slices"

	"go.mongodb.org/mongo-driver/v2/bson"
)

type Slice struct {
	Array    any
	Position *any
	N        any
}

func (s Slice) MarshalBSON() ([]byte, error) {
	args := []any{s.Array, s.N}
	if s.Position != nil {
		args = slices.Insert(args, 1, *s.Position)
	}

	return bson.Marshal(bson.D{
		{"$slice", args},
	})
}
