package util

import (
	"github.com/10gen/migration-verifier/mmongo"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/bson"
)

// ExcludePrefixesQuery returns a document that represents a query
// (not aggregation!) that matches documents where the indicated field
// DOES NOT have any of the indicated prefixes.
//
// This is useful, e.g., in $match aggregation phases to exclude change
// events for system namespaces.
func ExcludePrefixesQuery(fieldName string, prefixes []string) bson.D {
	return bson.D{
		{"$expr", bson.D{{"$not", bson.D{
			{"$or", lo.Map(
				prefixes,
				func(prefix string, _ int) bson.D {
					return mmongo.StartsWithAgg("$"+fieldName, prefix)
				},
			)},
		}}}},
	}
}
