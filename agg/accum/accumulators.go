// Package accum exposes helper types for accumulation operators.
package accum

import "go.mongodb.org/mongo-driver/v2/bson"

type Sum [1]any

var _ bson.Marshaler = Sum{}

func (s Sum) MarshalBSON() ([]byte, error) {
	return bson.Marshal(bson.D{{"$sum", s[0]}})
}

//----------------------------------------------------------------------

type Push [1]any

var _ bson.Marshaler = Push{}

func (p Push) MarshalBSON() ([]byte, error) {
	return bson.Marshal(bson.D{{"$push", p[0]}})
}

//----------------------------------------------------------------------

type Max [1]any

var _ bson.Marshaler = Max{}

func (m Max) MarshalBSON() ([]byte, error) {
	return bson.Marshal(bson.D{{"$max", m[0]}})
}

//----------------------------------------------------------------------

type FirstN struct {
	N     any
	Input any
}

var _ bson.Marshaler = FirstN{}

func (t FirstN) MarshalBSON() ([]byte, error) {
	return bson.Marshal(bson.D{
		{"$firstN", bson.D{
			{"n", t.N},
			{"input", t.Input},
		}},
	})
}

//----------------------------------------------------------------------

type TopN struct {
	N      any
	SortBy bson.D
	Output any
}

var _ bson.Marshaler = TopN{}

func (t TopN) MarshalBSON() ([]byte, error) {
	return bson.Marshal(bson.D{
		{"$topN", bson.D{
			{"n", t.N},
			{"sortBy", t.SortBy},
			{"output", t.Output},
		}},
	})
}
