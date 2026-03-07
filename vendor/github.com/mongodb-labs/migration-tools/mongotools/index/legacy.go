package index

import (
	"fmt"
	"math/big"

	"github.com/mongodb-labs/migration-tools/bsontools"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

var modernizedKeyElemValue = bsontools.ToRawValue(int32(1))

// ModernizeSpec modernizes pre-3.4 index key values. The server only applies stricter index key pattern validation to
// indexes created on 3.4+ versions (aka v2+ indexes) as of SERVER-26659. This means that the server does not apply stricter
// validation on pre-3.4 legacy indexes. Pre-3.4 index key values could be 0, empty string, or non-numeric / string values.
// 3.4+ server versions still handle pre-3.4 legacy indexes because these legacy indexes can persist across server upgrades.
// The server treats the following index key values as ascending (1):
// 1) non-negative numbers
// 2) non-numerical values
// 3) empty strings
// The server treats negative numerical index key values as descending (-1). Non-empty strings are treated specially (ex: "hashed").
// This is according to:
// https://github.com/10gen/mongo/tree/master/src/mongo/db/catalog#legacy-catalog-formats-that-still-require-support.
//
// To address this, the function converts a pre-v2 index key value to 1 if the value is 0, the empty string, or a non-numerical/string
// value. The function returns true if one or more index keys are converted, false if not. This logic should match the logic in
// the JS consistency checker.
//
// Note: indexes with NaN index key values are not handled. REP-5344 discusses changing this.
func ModernizeSpec(indexSpec bson.Raw) (bson.Raw, bool, error) {
	shouldModernize, err := shouldModernizeSpec(indexSpec)
	if err != nil {
		return nil, false, err
	}
	if !shouldModernize {
		return indexSpec, false, nil
	}

	keySpec, err := bsontools.RawLookup[bson.Raw](indexSpec, "key")
	if err != nil {
		return nil, false, fmt.Errorf("reading `key` from index spec (%+v): %w", indexSpec, err)
	}

	modernizedSpec, anythingWasModernized, err := getModernizedKeySpec(keySpec)
	if err != nil {
		return nil, false, fmt.Errorf(
			"modernizing index’s key spec (index: %+v): %w",
			indexSpec,
			err,
		)
	}

	if !anythingWasModernized {
		return indexSpec, false, nil
	}
	keySpec, err = bson.Marshal(modernizedSpec)
	if err != nil {
		return nil, false, fmt.Errorf(
			"re-marshaling modernized key spec (%+v): %w",
			modernizedSpec,
			err,
		)
	}

	var found bool
	newSpec, found, err := bsontools.ReplaceInRaw(indexSpec, bsontools.ToRawValue(keySpec), "key")
	if err != nil {
		return nil, false, fmt.Errorf("replacing index spec’s key with modernized: %w", err)
	}

	lo.Assertf(found, "must have found the key spec")

	return newSpec, true, nil
}

func getModernizedKeySpec(keySpec bson.Raw) (bson.D, bool, error) {
	var modernizedSpec bson.D
	var anythingWasModernized bool

	for keyElem, err := range bsontools.RawElements(keySpec) {
		if err != nil {
			return nil, false, fmt.Errorf("iterating key spec (%+v): %w", keySpec, err)
		}

		key, err := keyElem.KeyErr()
		if err != nil {
			return nil, false, fmt.Errorf("parsing field name key spec (%+v): %w", keySpec, err)
		}

		val, err := keyElem.ValueErr()
		if err != nil {
			return nil, false, fmt.Errorf(
				"parsing key spec’s %#q (spec: %+v): %w",
				key,
				keySpec,
				err,
			)
		}

		if shouldModernizeValue(val) {
			anythingWasModernized = true
			val = modernizedKeyElemValue
		}

		modernizedSpec = append(modernizedSpec, bson.E{key, val})
	}

	return modernizedSpec, anythingWasModernized, nil
}

// shouldModernizeSpec returns true if the index is pre-v2.
func shouldModernizeSpec(indexSpec bson.Raw) (bool, error) {

	// All the versions of the server that migrations support have a v field
	// in their indices.
	indexVersion, err := bsontools.RawLookup[int](indexSpec, "v")
	if err != nil {
		return false, fmt.Errorf(
			"extracting `v` from index spec (%+v): %w",
			indexSpec,
			err,
		)
	}

	switch indexVersion {
	case 1:
		return true, nil
	case 2:
		return false, nil
	}

	return false, fmt.Errorf(
		"index has an unexpected `v` value: %d",
		indexVersion,
	)
}

func shouldModernizeValue(value bson.RawValue) bool {
	switch value.Type {
	case bson.TypeInt32, bson.TypeInt64, bson.TypeDouble:
		return value.AsFloat64() == 0
	case bson.TypeDecimal128:
		if bi, _, err := value.Decimal128().BigInt(); err == nil {
			if bi.Cmp(big.NewInt(0)) == 0 {
				return true
			}
		}

		return false
	case bson.TypeString:
		return value.StringValue() == ""
	default:
		// Convert all types that aren't strings or numbers.
		return true
	}
}

func omitVersionFromIndexSpec(spec bson.Raw) (bson.Raw, error) {
	newSpec, _, err := bsontools.RemoveFromRaw(spec, "v")

	return newSpec, err
}
