package expr

// BSONType is a BSON type name as used in $type expressions. See the
// $type operator’s documentation for the canonical list of names:
// https://www.mongodb.com/docs/manual/reference/operator/aggregation/type/
type BSONType string

const (
	BSONTypeDouble              BSONType = "double"
	BSONTypeString              BSONType = "string"
	BSONTypeObject              BSONType = "object"
	BSONTypeArray               BSONType = "array"
	BSONTypeBinData             BSONType = "binData"
	BSONTypeUndefined           BSONType = "undefined"
	BSONTypeObjectID            BSONType = "objectId"
	BSONTypeBool                BSONType = "bool"
	BSONTypeDate                BSONType = "date"
	BSONTypeNull                BSONType = "null"
	BSONTypeRegex               BSONType = "regex"
	BSONTypeDBPointer           BSONType = "dbPointer"
	BSONTypeJavaScript          BSONType = "javascript"
	BSONTypeSymbol              BSONType = "symbol"
	BSONTypeJavaScriptWithScope BSONType = "javascriptWithScope"
	BSONTypeInt                 BSONType = "int"
	BSONTypeTimestamp           BSONType = "timestamp"
	BSONTypeLong                BSONType = "long"
	BSONTypeDecimal             BSONType = "decimal"
	BSONTypeMinKey              BSONType = "minKey"
	BSONTypeMaxKey              BSONType = "maxKey"

	// BSONTypeNumber is an alias that matches BSONTypeDouble,
	// BSONTypeInt, BSONTypeLong, and BSONTypeDecimal.
	BSONTypeNumber BSONType = "number"
)
