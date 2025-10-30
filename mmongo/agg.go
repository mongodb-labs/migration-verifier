package mmongo

import "go.mongodb.org/mongo-driver/v2/bson"

// StartsWithAgg returns an aggregation expression that indicates whether
// the referent field begins with the “besought” string.
//
// Equivalent to JavaScript String.prototype.startsWith(besought).
func StartsWithAgg(fieldRef string, besought string) bson.D {
	return bson.D{
		{"$eq", bson.A{
			0,
			bson.D{{"$indexOfCP", bson.A{
				fieldRef,
				besought,
				0, // start scanning at index 0
				1, // stop scanning at index 1
			}}},
		}},
	}
}
