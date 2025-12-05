package agg

import "go.mongodb.org/mongo-driver/v2/bson"

type Sum [1]any

var _ bson.Marshaler = Sum{}

func (s Sum) MarshalBSON() ([]byte, error) {
	return bson.Marshal(bson.D{{"$sum", s[0]}})
}
