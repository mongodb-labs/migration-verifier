// Package dockey contains logic related to document key determination.
// Its tests use a cluster and thus are stored in internal/verifier.

package dockey

import (
	"maps"
	"slices"
	"strconv"

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

	var docKeyNumKeys bson.D
	numToKeyLookup := map[string]string{}

	for n, name := range fieldNames {
		var valExpr = docExpr + "." + name

		// Aggregation forbids direct creation of an object with dotted keys.
		// So here we create an object with numeric keys, then below we’ll
		// map the numeric keys back to the real ones.

		nStr := strconv.Itoa(n)
		docKeyNumKeys = append(docKeyNumKeys, bson.E{nStr, valExpr})
		numToKeyLookup[nStr] = name
	}

	// Now convert the numeric keys back to the real ones.
	return mapObjectKeysAgg(docKeyNumKeys, numToKeyLookup)
}

// Potentially reusable:
func mapObjectKeysAgg(expr any, mapping map[string]string) bson.D {
	// We would ideally pass mapping into the aggregation and $getField
	// to get the mapped key, but pre-v8 server versions required $getField’s
	// field parameter to be a constant. (And pre-v5 didn’t have $getField
	// at all.) So we use a $switch instead.
	mapAgg := bson.D{
		{"$switch", bson.D{
			{"branches", lo.Map(
				slices.Collect(maps.Keys(mapping)),
				func(key string, _ int) bson.D {
					return bson.D{
						{"case", bson.D{
							{"$eq", bson.A{
								key,
								"$$numericKey",
							}},
						}},
						{"then", mapping[key]},
					}
				},
			)},
		}},
	}

	return bson.D{
		{"$arrayToObject", bson.D{
			{"$map", bson.D{
				{"input", bson.D{
					{"$objectToArray", expr},
				}},
				{"in", bson.D{
					{"$let", bson.D{
						{"vars", bson.D{
							{"numericKey", "$$this.k"},
							{"value", "$$this.v"},
						}},
						{"in", bson.D{
							{"k", mapAgg},
							{"v", "$$value"},
						}},
					}},
				}},
			}},
		}},
	}
}
