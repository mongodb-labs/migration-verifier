package partitions

import (
	"fmt"
	"slices"

	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// PartitionKey represents the _id of a partition document stored in the destination.
type PartitionKey struct {
	SourceUUID  util.UUID `bson:"srcUUID"`
	MongosyncID string    `bson:"id"`
	Lower       any       `bson:"lowerBound"`
}

// Namespace stores the database and collection name of the namespace being copied.
type Namespace struct {
	DB   string `bson:"db"`
	Coll string `bson:"coll"`
}

// Partition represents a range of documents in a namespace, bounded by the _id field.
//
// A valid partition must have a non-nil lower bound (in its PartitionKey) and a non-nil upper bound.
type Partition struct {
	Key PartitionKey `bson:"_id"`
	Ns  *Namespace   `bson:"namespace"`

	// The upper index key bound for the partition.
	Upper any `bson:"upperBound"`

	// Set to true if the partition is for a capped collection. If so, this partition's
	// upper/lower bounds should be set to the minKey and maxKey of the collection.
	IsCapped bool `bson:"isCapped"`
}

// String returns a string representation of the partition.
func (p *Partition) String() string {
	return fmt.Sprintf(
		"{db: %s, coll: %s, collUUID: %s, mongosyncID: %s, lower: %s, upper: %s}",
		p.Ns.DB, p.Ns.Coll, p.Key.SourceUUID, p.Key.MongosyncID, p.GetLowerBoundString(), p.GetUpperBoundString())
}

// GetLowerBoundString returns the string representation of this partition's lower bound.
func (p *Partition) GetLowerBoundString() string {
	return p.getIndexKeyBoundString(p.Key.Lower)
}

// GetUpperBoundString returns the string representation of this partition's upper bound.
func (p *Partition) GetUpperBoundString() string {
	return p.getIndexKeyBoundString(p.Upper)
}

// getIndexKeyBoundString returns the string representation of the given index key bound.
func (p *Partition) getIndexKeyBoundString(bound any) string {
	switch b := bound.(type) {
	case bson.RawValue:
		return b.String()
	case primitive.MinKey:
		return `{"$minKey":1}`
	case primitive.MaxKey:
		return `{"$maxKey":1}`
	default:
		return fmt.Sprintf("%v", b)
	}
}

type PartitionQueryParameters struct {
	filter    option.Option[bson.D]
	sortField option.Option[string]
	hint      option.Option[bson.D]
}

func (pqp PartitionQueryParameters) ToFindOptions() bson.D {
	doc := bson.D{}

	if theFilter, has := pqp.filter.Get(); has {
		doc = append(doc, bson.E{"filter", theFilter})
	}

	pqp.addHintIfNeeded(&doc)

	return doc
}

func (pqp PartitionQueryParameters) ToAggOptions() bson.D {
	pl := mongo.Pipeline{}

	if theFilter, has := pqp.filter.Get(); has {
		pl = append(pl, bson.D{{"$match", theFilter}})
	}

	if theSort, has := pqp.sortField.Get(); has {
		pl = append(pl, bson.D{{"$sort", bson.D{{theSort, 1}}}})
	}

	doc := bson.D{
		{"pipeline", pl},
	}

	pqp.addHintIfNeeded(&doc)

	return doc
}

func (pqp PartitionQueryParameters) addHintIfNeeded(docRef *bson.D) {
	if theHint, has := pqp.hint.Get(); has {
		*docRef = append(*docRef, bson.E{"hint", theHint})
	}
}

// GetQueryParameters returns a PartitionQueryParameters that describes the
// parameters needed to fetch docs for the partition. It is intended to allow
// the same partitioning to be used on different collections (e.g. use the
// partitions on the source to read the destination for verification)
// If the passed-in buildinfo indicates a mongodb version < 5.0, type bracketing is not used.
// filterAndPredicates is a slice of filter criteria that's used to construct the "filter" field in the find option.
func (p *Partition) GetQueryParameters(clusterInfo *util.ClusterInfo, filterAndPredicates bson.A) (PartitionQueryParameters, error) {
	params := PartitionQueryParameters{}

	if p == nil {
		if len(filterAndPredicates) > 0 {
			params.filter = option.Some(bson.D{{"$and", filterAndPredicates}})
		}

		return params, nil
	}

	if p.IsCapped {
		// For capped collections, sort the documents by their natural order. We deliberately
		// exclude the ID filter to ensure that documents are inserted in the correct order.
		params.sortField = option.Some("$natural")
	} else {
		// For non-capped collections, sort by _id to minimize the amount of time
		// that a given document spends cached in memory.
		params.sortField = option.Some("_id")

		newPredicates, err := FilterIdBounds(clusterInfo, p.Key.Lower, p.Upper)
		if err != nil {
			return PartitionQueryParameters{}, err
		}

		// For non-capped collections, the cursor should use the ID filter and the _id index.
		// Get the bounded query filter from the partition to be used in the Find command.
		filterAndPredicates = append(
			slices.Clone(filterAndPredicates),
			lo.ToAnySlice(newPredicates)...,
		)

		params.hint = option.Some(bson.D{{"_id", 1}})
	}

	if len(filterAndPredicates) > 0 {
		params.filter = option.Some(bson.D{{"$and", filterAndPredicates}})
	}

	return params, nil
}

// FilterIdBounds returns a slice of query predicates that, when ANDed together,
// effect a non-type-bracketed query for all values between the given boundaries
// (inclusive). The query will NOT require fetching full documents.
func FilterIdBounds(clusterInfo *util.ClusterInfo, lower, upper any) ([]bson.D, error) {
	// For non-capped collections, the cursor should use the ID filter and the _id index.
	// Get the bounded query filter from the partition to be used in the Find command.
	useExprFind := true

	if clusterInfo != nil {
		useExprFind = false

		if clusterInfo.VersionArray != nil {
			useExprFind = clusterInfo.VersionArray[0] >= 5
		}
	}

	getPredicatesFunc := lo.Ternary(useExprFind, getExprPredicates, getExplicitTypeCheckPredicates)
	return getPredicatesFunc(lower, upper)
}

// This returns a range filter on _id to be used in a Find query for the
// partition.  This filter will properly handle mixed-type _ids, but on server versions
// < 5.0, will not use indexes and thus will be very slow.
func getExprPredicates(lower, upper any) ([]bson.D, error) {
	// We use $expr to avoid type bracketing and allow comparison of different _id types,
	// and $literal to avoid MQL injection from an _id's value.
	predicates := []bson.D{{
		{"$expr", bson.D{
			{"$gte", bson.A{
				"$_id",
				bson.D{{"$literal", lower}},
			}},
		}},
	}}

	// All _id values <= upper bound.
	predicates = append(
		predicates,
		bson.D{{"$expr", bson.D{
			{"$lte", bson.A{
				"$_id",
				bson.D{{"$literal", upper}},
			}},
		}}},
	)

	return predicates, nil
}

// getExplicitTypeCheckPredicates compensates for the server’s type bracketing
// by matching _id types between the partition’s min & max boundaries.
// It should yield the same result as filterWithExpr.
func getExplicitTypeCheckPredicates(lower, upper any) ([]bson.D, error) {
	_, betweenTypes, err := getTypeBracketExcludedBSONTypes(lower)
	if err != nil {
		return nil, errors.Wrapf(err, "getting types above lower bound (%T: %v)", lower, lower)
	}

	var rangePredicate bson.D
	var orPredicates []bson.D

	lowerRV, err := mbson.ConvertToRawValue(lower)
	if err != nil {
		return nil, errors.Wrapf(err, "converting lower bound (%T: %v) to %T", lower, lower, lowerRV)
	}
	if lowerRV.Type != bson.TypeMinKey {
		rangePredicate = bson.D{{"_id", bson.D{{"$gte", lower}}}}
	}

	upperRV, err := mbson.ConvertToRawValue(upper)
	if err != nil {
		return nil, errors.Wrapf(err, "converting upper bound (%T: %v) to %T", upper, upper, upperRV)
	}

	if upperRV.Type != bson.TypeMaxKey {
		typesBelowUpper, typesAboveUpper, err := getTypeBracketExcludedBSONTypes(upper)
		if err != nil {
			return nil, errors.Wrapf(err, "getting types around upper bound (%T: %v)", upper, upper)
		}

		if slices.Contains(typesAboveUpper, lowerRV.Type) {
			return nil, fmt.Errorf(
				"lower bound’s BSON type (%s) sorts later than upper bound’s (%s)",
				lowerRV.Type,
				upperRV.Type,
			)
		}

		bothLimitsPredicate := mslices.Compact(
			mslices.Of(
				rangePredicate,
				bson.D{{"_id", bson.D{{"$lte", upper}}}},
			),
		)

		if slices.Contains(typesBelowUpper, lowerRV.Type) {
			betweenTypes = lo.Intersect(betweenTypes, typesBelowUpper)
			// If the limits’ types don’t sort together, then we OR the
			// predicates together. This is correct because, in a non-$expr
			// query, “foo > 5” means “foo > 5 AND foo is numeric”.
			orPredicates = bothLimitsPredicate
		} else {
			// If the limits sort together, then we AND them together.
			return bothLimitsPredicate, nil
		}
	} else {
		orPredicates = mslices.Of(rangePredicate)
	}

	if len(betweenTypes) > 0 {

		orPredicates = append(
			orPredicates,
			bson.D{{"_id", bson.D{{"$type", typesToStrings(betweenTypes)}}}},
		)
	}

	return []bson.D{{{"$or", mslices.Compact(orPredicates)}}}, nil
}
