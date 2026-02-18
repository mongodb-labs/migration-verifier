// Package mongotools contains tools for use with MongoDB servers.
//
// See the `bsontools` package for BSON-related tools.
package mongotools

import mapset "github.com/deckarep/golang-set/v2"

// Taken from:
// https://github.com/mongodb/mongo/blob/master/src/mongo/db/index/index_descriptor.h
var validIndexOptions = mapset.NewSet(
	"2dsphereIndexVersion",
	"background",
	"bits",
	"bucketSize",
	"coarsestIndexedLevel",
	"collation",
	"default_language",
	"expireAfterSeconds",
	"finestIndexedLevel",
	"key",
	"language_override",
	"max",
	"min",
	"name",
	"ns",
	"partialFilterExpression",
	"sparse",
	"storageEngine",
	"textIndexVersion",
	"unique",
	"v",
	"weights",
	"wildcardProjection",
)

func GetValidIndexOptions() mapset.Set[string] {
	return validIndexOptions.Clone()
}
