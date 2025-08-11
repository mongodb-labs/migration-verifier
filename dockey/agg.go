package dockey

import (
	"strings"

	"go.mongodb.org/mongo-driver/bson"
)

func ExtractDocKeyAgg(fieldNames []string) bson.D {
	return extractDocKeyFromAgg(fieldNames, "$$ROOT")
}

func extractDocKeyFromAgg(fieldNames []string, rootExpr any) bson.D {
	var docKeyPieces [][2]any

	for _, name := range fieldNames {
		var valExpr = extractKeyValueAgg(name, rootExpr)

		docKeyPieces = append(docKeyPieces, [...]any{name, valExpr})
	}

	return bson.D{
		{"$arrayToObject", bson.A{docKeyPieces}},
	}
}

func existsAgg(fieldName string, baseDocExpr any) bson.D {
	base, remainder, hasDot := strings.Cut(fieldName, ".")

	fieldExpr := bson.D{
		{"$getField", bson.D{
			{"field", fieldName},
			{"input", baseDocExpr},
		}},
	}

	if !hasDot {
		return bson.D{
			{"$ne", bson.A{
				"missing",
				bson.D{{"$type", fieldExpr}},
			}},
		}
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
			{"then", true},
			{"else", bson.D{
				{"$cond", bson.D{
					{"if", bson.D{
						{"$eq", bson.A{
							"object",
							bson.D{{"$type", embDocExpr}},
						}},
					}},
					{"then", extractKeyValueAgg(remainder, embDocExpr)},
					{"else", false},
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
