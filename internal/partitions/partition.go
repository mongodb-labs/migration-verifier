package partitions

import (
	"fmt"
	"slices"
	"time"

	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/mongodb-labs/migration-tools/option"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

func ForPartitionAggregation(coll *mongo.Collection) *mongo.Collection {
	return coll.
		Database().
		Collection(
			coll.Name(),
			options.Collection().
				SetReadConcern(readconcern.Available()).
				SetReadPreference(
					readpref.SecondaryPreferred(readpref.WithMaxStaleness(90*time.Second))),
		)
}

// PartitionKey represents the _id of a partition document stored in the destination.
type PartitionKey struct {
	SourceUUID util.UUID `bson:"srcUUID"`

	// The partition’s lower bound.
	// Under ID partitioning this is always given.
	// Under natural partitioning this is null (or empty) in the first partition.
	Lower bson.RawValue `bson:"lowerBound"`
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

	// The partition’s upper bound.
	// Unlike the lower bound, this always exists, even in natural partitioning.
	Upper bson.RawValue `bson:"upperBound"`

	// Set to true if the partition is for a capped collection. If so, this partition's
	// upper/lower bounds should be set to the minKey and maxKey of the collection.
	IsCapped bool `bson:"isCapped"`

	Natural bool

	// The hostname to use when querying in $natural order.
	HostnameAndPort option.Option[string]
}

// String returns a string representation of the partition.
func (p *Partition) String() string {
	return fmt.Sprintf(
		"{db: %s, coll: %s, collUUID: %s, lower: %s, upper: %s}",
		p.Ns.DB, p.Ns.Coll, p.Key.SourceUUID, p.GetLowerBoundString(), p.GetUpperBoundString())
}

// GetLowerBoundString returns the string representation of this partition's lower bound.
func (p *Partition) GetLowerBoundString() string {
	return p.Key.Lower.String()
}

// GetUpperBoundString returns the string representation of this partition's upper bound.
func (p *Partition) GetUpperBoundString() string {
	return p.Upper.String()
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

	if theHint, has := pqp.hint.Get(); has {
		doc = append(doc, bson.E{"hint", theHint})
	}

	if sortField, has := pqp.sortField.Get(); has {
		doc = append(doc, bson.E{"sort", bson.D{{sortField, 1}}})
	}

	return doc
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
func FilterIdBounds(clusterInfo *util.ClusterInfo, lower, upper bson.RawValue) ([]bson.D, error) {
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
func getExprPredicates(lower, upper bson.RawValue) ([]bson.D, error) {
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
// It should yield the same result as getExprPredicates.
func getExplicitTypeCheckPredicates(lower, upper bson.RawValue) ([]bson.D, error) {
	_, typesAboveLower, err := getTypeBracketExcludedBSONTypes(lower)
	if err != nil {
		return nil, errors.Wrapf(err, "getting types above lower bound (%s: %v)", lower.Type, lower)
	}

	typesBelowUpper, typesAboveUpper, err := getTypeBracketExcludedBSONTypes(upper)
	if err != nil {
		return nil, errors.Wrapf(err, "getting types below upper bound (%s: %v)", upper.Type, upper)
	}

	limitPredicates := mslices.Of(
		boundPredicate("$gte", lower),
		boundPredicate("$lte", upper),
	)

	switch {
	case slices.Contains(typesAboveUpper, lower.Type):
		return nil, fmt.Errorf(
			"lower bound’s BSON type (%s) sorts later than upper bound’s (%s)",
			lower.Type,
			upper.Type,
		)
	case !slices.Contains(typesBelowUpper, lower.Type):
		// The bounds share a type bracket, so AND them.
		return limitPredicates, nil
	}

	// The bounds are in different type brackets. Bracketing confines each
	// limit to its own bracket, so OR them, plus any brackets in between.
	orPredicates := limitPredicates
	if between := lo.Intersect(typesAboveLower, typesBelowUpper); len(between) > 0 {
		orPredicates = append(
			orPredicates,
			bson.D{{"_id", bson.D{{"$type", typesToStrings(between)}}}},
		)
	}

	return []bson.D{{{"$or", orPredicates}}}, nil
}

// boundPredicate returns {_id: {<op>: bound}}, except for MinKey & MaxKey.
// Comparisons with those aren’t type-bracketed and so would match every _id;
// for those we match the bound itself instead.
func boundPredicate(op string, bound bson.RawValue) bson.D {
	if bound.Type == bson.TypeMinKey || bound.Type == bson.TypeMaxKey {
		return bson.D{{"_id", bound}}
	}

	return bson.D{{"_id", bson.D{{op, bound}}}}
}
