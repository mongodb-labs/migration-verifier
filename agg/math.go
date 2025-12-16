package agg

import "go.mongodb.org/mongo-driver/v2/bson"

type Subtract [2]any

var _ bson.Marshaler = Subtract{}

func (s Subtract) MarshalBSON() ([]byte, error) {
	return bson.Marshal(bson.D{{"$subtract", [2]any(s)}})
}
