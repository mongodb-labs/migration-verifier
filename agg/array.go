package agg

import "go.mongodb.org/mongo-driver/v2/bson"

type Size [1]any

var _ bson.Marshaler = Size{}

func (s Size) MarshalBSON() ([]byte, error) {
	return bson.Marshal(bson.D{{"$size", s[0]}})
}
