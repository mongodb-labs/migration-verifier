// Package dockey contains logic related to document key determination.
// Its tests use a cluster and thus are stored in internal/verifier.

package dockey

import (
	"github.com/10gen/migration-verifier/mslices"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/bson"
)

// ExtractTrueDocKeyAgg returns an aggregation expression that extracts the
// document key from the document to which the `docExpr` refers.
//
// NB: This avoids the problem documented in SERVER-109340; as a result,
// the returned key may not always match the change stream’s `documentKey`
// (because the server misreports its own sharding logic).
func ExtractTrueDocKeyAgg(fieldNames []string, docExpr string) bson.D {
	assertFieldNameUniqueness(fieldNames)

	return bson.D{
		{"$arrayToObject", mslices.Of(lo.Map(
			fieldNames,
			func(fieldName string, _ int) [2]string {
				return [...]string{fieldName, docExpr + "." + fieldName}
			},
		))},
	}
}
