// Package helpers exposes functions that express common operations
// that don’t map to a single aggregation operator.
package helpers

import (
	"github.com/10gen/migration-verifier/agg"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type Exists [1]any

func (e Exists) MarshalBSON() ([]byte, error) {
	return bson.Marshal(agg.Not{agg.Eq{"missing", agg.Type{e[0]}}})
}
