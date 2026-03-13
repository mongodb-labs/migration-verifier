// Package index exports index-related functions.
package index

import (
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/ccoveille/go-safecast/v2"
	mapset "github.com/deckarep/golang-set/v2"
	"github.com/mongodb-labs/migration-tools/bsontools"
	"github.com/mongodb-labs/migration-tools/option"
	"github.com/samber/lo"
	"github.com/wI2L/jsondiff"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/x/bsonx/bsoncore"
)

// These index options must be compared with sensitivity to field order.
var optsWhereOrderMatters = []string{
	"key",

	// NB: This is here in case any filters contain embedded documents, which
	// MongoDB compares order-sensitively.
	"partialFilterExpression",
}

var optsToIgnore = mapset.NewSet(
	// We can ignore the `v` field when comparing indexes. This is because:
	// - We already validated that the version is v1 or v2.
	// - There are no backwards-incompatible features between v1 & v2 indexes.
	//   (v2 indexes only added `NumberDecimal` and `Collation`.)
	"v",

	// v4.4 stopped adding “ns” to index fields.
	"ns",

	// v4.2+ ignores this field.
	"background",
)

// SpecDiff describes the difference between two index specifications.
type SpecDiff struct {
	// JSONPatch is a diff between the two indexes in ext JSON. It won’t
	// reflect significant field order differences.
	JSONPatch jsondiff.Patch

	// FieldOrderDiffers gives all options whose values differ by
	// field order alone.
	FieldOrderDiffers []string

	jsonPatchErr error
}

func (sd SpecDiff) String() string {
	if sd.jsonPatchErr != nil {
		return fmt.Sprintf(
			"specs differ; failed to create ext JSON patch (%v)",
			sd.jsonPatchErr,
		)
	}

	if sd.JSONPatch != nil {
		patch := sd.JSONPatch

		patchStr, err := fixJSONPatchFieldOrder([]byte(patch.String()))
		if err != nil {
			patchStr = fmt.Appendf(nil, "%s (failed to normalize order: %v)", patch.String(), err)
		}

		return string(patchStr)
	}

	return fmt.Sprintf("field order differs: %#q", sd.FieldOrderDiffers)
}

// DescribeSpecDifferences compares two index specifications and returns a
// human-readable description of the mismatch. It:
// 1) normalizes legacy index specifications
// 2) omits the version field
// 3) correctly considers or ignores field order as appropriate.
func DescribeSpecDifferences(specA, specB bson.Raw) (option.Option[SpecDiff], error) {
	specA = slices.Clone(specA)
	specB = slices.Clone(specB)

	specA, err := prepareIndexSpecForEqualityCheck(specA)
	if err != nil {
		return option.None[SpecDiff](), err
	}

	specB, err = prepareIndexSpecForEqualityCheck(specB)
	if err != nil {
		return option.None[SpecDiff](), err
	}

	equalNoOrder, err := equalIgnoringOrder(specA, specB)
	if err != nil {
		return option.None[SpecDiff](), err
	}

	if !equalNoOrder {
		specAExtJSON, err := bson.MarshalExtJSON(specA, true, false)
		if err != nil {
			return option.Some(SpecDiff{
				jsonPatchErr: fmt.Errorf("marshal spec A to ext JSON: %w", err),
			}), nil
		}

		specBExtJSON, err := bson.MarshalExtJSON(specB, true, false)
		if err != nil {
			return option.Some(SpecDiff{
				jsonPatchErr: fmt.Errorf("marshal spec B to ext JSON: %w", err),
			}), nil
		}

		patch, err := jsondiff.CompareJSON(
			specAExtJSON,
			specBExtJSON,
			// jsondiff.Factorize(), // https://github.com/wI2L/jsondiff/issues/45
		)

		return option.Some(SpecDiff{
			jsonPatchErr: err,
			JSONPatch:    patch,
		}), nil
	}

	orderDifferFields, err := getOrderDifferOpts(specA, specB)
	if err != nil {
		return option.None[SpecDiff](), fmt.Errorf(
			"compare order-sensitive index spec fields: %w",
			err,
		)
	}

	if len(orderDifferFields) == 0 {
		return option.None[SpecDiff](), nil
	}

	return option.Some(SpecDiff{
		FieldOrderDiffers: orderDifferFields,
	}), nil
}

func prepareIndexSpecForEqualityCheck(spec bson.Raw) (bson.Raw, error) {
	spec, _, err := ModernizeSpec(spec)
	if err != nil {
		return nil, fmt.Errorf("modernizing: %w", err)
	}

	spec, err = normalizeTypesInSpec(spec)
	if err != nil {
		return nil, fmt.Errorf("normalizing types: %w", err)
	}

	for field := range optsToIgnore.Iter() {
		spec, _, err = bsontools.RemoveFromRaw(spec, field)
		if err != nil {
			return nil, err
		}
	}

	return spec, nil
}

// This assumes the specs have been through prepareIndexSpecForEqualityCheck.
func getOrderDifferOpts(specA, specB bson.Raw) ([]string, error) {
	var orderDifferOpts []string

	for _, optName := range optsWhereOrderMatters {
		var optValueA, optValueB bson.RawValue

		// NB: By this point we know the specs match when ignoring order.
		// That means that A & B have the same keys. Thus, we only need to
		// check existence in one of the specs.
		optValueA, err := specA.LookupErr(optName)
		if err != nil {
			if errors.Is(err, bsoncore.ErrElementNotFound) {
				continue
			}

			return nil, fmt.Errorf(
				"look up %#q in spec A (%v): %w",
				optName,
				specA,
				err,
			)
		}

		optValueB, err = specB.LookupErr(optName)
		if err != nil {
			if errors.Is(err, bsoncore.ErrElementNotFound) {
				continue
			}

			return nil, fmt.Errorf(
				"look up %#q in spec B (%v): %w",
				optName,
				specB,
				err,
			)
		}

		if !optValueA.Equal(optValueB) {
			orderDifferOpts = append(orderDifferOpts, optName)
		}
	}

	return orderDifferOpts, nil
}

// The server stores certain index values differently from how they’re
// actually used. `expireAfterSeconds`, for example, gets stored as a
// double even though it’s always used as a long (i32). Thus, if you create
// an index with expireAfterSeconds=123.4, it’ll be used as 123, even
// though the server stores 123.4 internally.
//
// Some commands, such as `$indexStats` and `$listCatalog`, report internal
// values. That means it’s possible for two shards to have different
// “internal” values (e.g., 123.4 vs. 123) that actually get used the
// same way and, thus, don’t conflict in practice.
//
// The function below converts the stored/internal index properties to their
// “active” equivalents. This is the Go equivalent of NormalizeIndexDefinitionsStage().
// We decided to normalize indexes here as well as add it to our $listCatalog and $indexStats
// pipelines in case a future maintainer does not use our pre-defined pipelines to fetch indexes
// and compares them, in which case there may be a mismatch that would not exist had the indexes
// been normalized.
//
// For a table with all known type differences, see:
// https://github.com/10gen/mongo/blob/master/src/mongo/db/catalog/README.md#examples-of-differences-between-listindexes-and-listcatalog-results
func normalizeTypesInSpec(spec bson.Raw) (bson.Raw, error) {
	spec, err := convertTTLSpecToInt32(spec)
	if err != nil {
		return nil, err
	}

	spec, err = convertBitsSpecToInt32(spec)
	if err != nil {
		return nil, err
	}

	spec, err = convertSparseSpecToBool(spec)
	if err != nil {
		return nil, err
	}

	return spec, nil
}

// convertTTLSpecToInt32 ensures that TTL values are not greater than math.MaxInt32 and then converts the TTL to an int32.
// TTL values must be within 0 and math.MaxInt32 according to these docs:
// https://www.mongodb.com/docs/manual/core/index-ttl/#create-a-ttl-index. (Pre-5.0 versions do not enforce that TTl values
// are less than math.MaxInt32.) If the spec lacks a TTL value, the function returns without modifying the spec. This
// function returns an error if it sees an unexpected TTL value.
//
// This is a workaround for SERVER-91498 (a TTL value can be stored as an int, long, or decimal on the server). For example,
// if you create an index with a TTL value, the TTL is stored as an int32 on the server. If you collMod an index with a
// TTL value, the TTL is capped at MaxInt32 and stored as an int64 on the server.
func convertTTLSpecToInt32(spec bson.Raw) (bson.Raw, error) {
	return convertToInt32(spec, "expireAfterSeconds", math.MaxInt32)
}

// convertBitsSpecToInt32 is a workaround for SERVER-73442 where the server will accept a non-integer `bits` value during `createIndexes` even
// though it is stored as an int under the hood. It is safe to convert it to an int32 since `bits` can only take values of 1 to 32 inclusive.
// For more details, see: https://www.mongodb.com/docs/manual/core/indexes/index-types/geospatial/2d/create/define-location-precision/
func convertBitsSpecToInt32(spec bson.Raw) (bson.Raw, error) {
	return convertToInt32(spec, "bits", 32)
}

// convertToInt32 converts the value of the given key in the spec to an int32. It expects the original value of the key
// to be numeric. Additionally, this function should only be used for index spec values in the server that fit within a
// 32-bit signed integer range. This is because the server uses static_cast<int> in C++ to perform $toInt which can result
// in undefined behavior on overflow whereas Go does well-defined truncation when converting to int32. For more details, see:
// https://github.com/10gen/mongo/blob/84ff3493467477ffee5b92b663622c843d06fd9e/src/mongo/db/exec/expression/evaluate_math.cpp#L1158
func convertToInt32(spec bson.Raw, keyName string, maxBound int32) (bson.Raw, error) {
	val, err := spec.LookupErr(keyName)
	if err != nil {
		if errors.Is(err, bsoncore.ErrElementNotFound) {
			return spec, nil
		}

		return nil, fmt.Errorf("extracting %#q: %w", keyName, err)
	}

	switch val.Type {
	case bson.TypeInt32:
		return spec, nil
	case bson.TypeInt64, bson.TypeDouble:
		if val.AsFloat64() > float64(maxBound) {
			return nil, fmt.Errorf(
				"%#q value (%f) cannot exceed %d",
				keyName,
				val.AsFloat64(),
				maxBound,
			)
		}
	default:
		return nil, fmt.Errorf(
			"expected %#q to be numeric but got %s",
			keyName,
			val.Type,
		)
	}

	i32, err := safecast.Convert[int32](val.AsFloat64())
	if err != nil {
		return nil, fmt.Errorf(
			"%#q value (%v) must be expressible as int32",
			keyName,
			val,
		)
	}

	newSpec, found, err := bsontools.ReplaceInRaw(
		spec,
		bsontools.ToRawValue(i32),
		keyName,
	)

	// We shouldn’t be here if keyName isn’t in the spec.
	lo.Assert(found, "must have found %#q", keyName)

	return newSpec, err
}

// Since the server allows numeric values for `sparse` but stores it as a bool, we normalize by converting all numeric
// `sparse` values to bools. This will error if the `sparse` value is non-numeric.
func convertSparseSpecToBool(spec bson.Raw) (bson.Raw, error) {
	keyName := "sparse"

	val, err := spec.LookupErr(keyName)
	if err != nil {
		if errors.Is(err, bsoncore.ErrElementNotFound) {
			return spec, nil
		}

		return nil, fmt.Errorf("extracting %#q: %w", keyName, err)
	}

	switch val.Type {
	case bson.TypeBoolean:
		return spec, nil
	case bson.TypeInt32, bson.TypeInt64, bson.TypeDouble:
	default:
		// The server does not allow non-numeric / bool types for `sparse` values.
		return nil, fmt.Errorf(
			"expected `sparse` to be a number or a bool; found %s",
			val.Type,
		)
	}

	newSpec, found, err := bsontools.ReplaceInRaw(
		spec,
		bsontools.ToRawValue(val.AsFloat64() != 0),
		keyName,
	)

	lo.Assert(found, "must have found %#q", keyName)

	return newSpec, err
}
