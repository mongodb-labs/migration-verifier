package util

import (
	"github.com/10gen/migration-verifier/mmongo"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/bson"
)

func ExcludePrefixesQuery(fieldSpec string, prefixes []string) bson.D {
	return bson.D{
		{"$expr", bson.D{{"$not", bson.D{
			{"$or", lo.Map(
				prefixes,
				func(prefix string, _ int) bson.D {
					return mmongo.StartsWithAgg("$"+fieldSpec, prefix)
				},
			)},
		}}}},
	}
}
