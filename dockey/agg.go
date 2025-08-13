// Package dockey contains logic related to document key determination.

package dockey

import (
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
)

// ExtractDocKeyAgg returns an aggregation expression that extracts the
// document key from the document to which the `docExpr` refers.
//
// For example, if the doc key field names are [`_id`, `lastName`], this
// function’s returned expression will yield the same document that shows
// as `documentKey` in the change stream if that document is inserted.
// Any fields missing in the document will be excluded from the document key.
//
// This correctly reproduces the server’s behavior when field names
// contain dots: it tries the full field name, then pops a “level” off
// the name & checks a level further in the document (again privileging
// the full field name’s remainder), and so on. For example, if the field
// name is `foo.bar.baz`, this will look for these fields in this order
// (JSON pointer notation):
//   - `/foo.bar.baz`
//   - `/foo/bar.baz`
//   - `/foo/bar/baz`
//
// Note that the above DOES NOT include `/foo.bar/baz`.
func ExtractDocKeyAgg(fieldNames []string, docExpr any) bson.D {
	var docKeyNumKeys bson.D
	numToKeyLookup := map[string]string{}

	for n, name := range fieldNames {
		var valExpr = extractKeyValueAgg(name, docExpr)

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
	return bson.D{
		{"$arrayToObject", bson.D{
			{"$map", bson.D{
				{"input", bson.D{
					{"$objectToArray", expr},
				}},
				{"in", bson.D{
					{"$let", bson.D{
						{"vars", bson.D{
							{"keyLookup", mapping},
						}},
						{"in", bson.D{
							{"k", bson.D{
								{"$getField", bson.D{
									{"field", "$$this.k"},
									{"input", "$$keyLookup"},
								}},
							}},
							{"v", "$$this.v"},
						}},
					}},
				}},
			}},
		}},
	}
}

func extractKeyValueAgg(fieldName string, baseDocExpr any) any {
	base, remainder, hasDot := strings.Cut(fieldName, ".")

	fieldExpr := bson.D{
		{"$getField", bson.D{
			{"field", fieldName},
			{"input", baseDocExpr},
		}},
	}

	if !hasDot {
		return fieldExpr
	}

	embDocExpr := bson.D{
		{"$getField", bson.D{
			{"field", base},
			{"input", baseDocExpr},
		}},
	}

	return bson.D{
		{"$cond", bson.D{
			{"if", bson.D{
				{"$ne", bson.A{
					"missing",
					bson.D{{"$type", fieldExpr}},
				}},
			}},
			{"then", fieldExpr},
			{"else", bson.D{
				{"$cond", bson.D{
					{"if", bson.D{
						{"$eq", bson.A{
							"object",
							bson.D{{"$type", embDocExpr}},
						}},
					}},
					{"then", extractKeyValueAgg(remainder, embDocExpr)},
					{"else", "$$REMOVE"},
				}},
			}},
		}},
	}
}
