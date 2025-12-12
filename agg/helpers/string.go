// Package helpers exposes functions that express common operations
// that don’t map to a single aggregation operator.
package helpers

import (
	"go.mongodb.org/mongo-driver/v2/bson"
)

// StringHasPrefix parallels Go’s strings.HasPrefix.
type StringHasPrefix struct {
	FieldRef any
	Prefix   string
}

func (sp StringHasPrefix) MarshalBSON() ([]byte, error) {
	return bson.Marshal(bson.D{
		{"$eq", bson.A{
			0,
			bson.D{{"$indexOfCP", bson.A{
				sp.FieldRef,
				sp.Prefix,
				0,
				1,
			}}},
		}},
	})
}
