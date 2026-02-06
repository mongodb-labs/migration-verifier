package agg

import "go.mongodb.org/mongo-driver/v2/bson"

type ToString [1]any

var _ bson.Marshaler = ToString{}

func (ts ToString) MarshalBSON() ([]byte, error) {
	return bson.Marshal(bson.D{{"$toString", ts[0]}})
}
