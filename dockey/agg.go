package dockey

import (
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
)

// This function is important enough that we test it directly.
// NOTE: The document contains base64-encoded names.
func ExtractDocKeyAgg(fieldNames []string) bson.D {
	return extractDocKeyFromAgg(fieldNames, "$$ROOT")
}

func extractDocKeyFromAgg(fieldNames []string, rootExpr any) bson.D {
	var docKeyNumKeys bson.D
	numToKeyLookup := map[string]string{}

	for n, name := range fieldNames {
		var valExpr = extractKeyValueAgg(name, rootExpr)

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
