package partitions

import (
	"context"
	"fmt"
	"math/rand"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/reportutils"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/internal/uuidutil"
	"github.com/10gen/migration-verifier/mmongo"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

const (
	//
	// In order for $sample to use a pseudo-random cursor (good) instead of doing a collection scan (bad), the
	// number of documents for $sample to fetch must be <5% of the total number of documents in the collection.
	// See: https://docs.mongodb.com/manual/reference/operator/aggregation/sample/#behavior
	//
	// We'd like to sample closer to 5% since, in theory, that gives us a better spread of documents to use
	// as partition boundaries. We choose to sample 4% of the collection for safety, in case the number of
	// documents increases between us getting the document count and us doing the actual sampling. It's still
	// possible that $sample does a collection scan if the number of documents increases very quickly, but
	// that should be very rare.
	//
	defaultSampleRate = 0.04

	//
	// The minimum number of documents $sample requires in order to use a pseudo-random cursor.
	// See: https://docs.mongodb.com/manual/reference/operator/aggregation/sample/#behavior
	//
	defaultSampleMinNumDocs = 101

	//
	// The maximum number of documents to sample per partition. Previously this is set to 10.
	// Because sampling too many documents can cause the WiredTiger bug (WT-13310),
	// it is reduced to 3 in REP-5971.
	//
	defaultMaxNumDocsToSamplePerPartition = 3

	//
	// In general, this constant should be set to (16 MB) / (defaultSampleRate) = (16 MB) / (4%) = 400 MB.
	// This is the smallest guaranteeable average partition size for the scenario where each document is
	// the maximum allowed size of 16 MB. Proof:
	//
	//   - Assume each document is the maximum allowed size of 16 MB.
	//   - We always sample 4% of the documents in a collection, or 1 out of every 25 documents.
	//   - Minimizing the partition sizes means maximizing the number of partitions, i.e. using
	//     every sampled document as a partition bound.
	//   - So each partition contains 25 documents, on average.
	//   - Average partition size = (average doc size) * (average number of docs per partition)
	//                            = (16 MB / doc)      * (25 docs / partition)
	//                            = (400 MB / partition)
	//
	// If this constant is set to less than 400 MB for a 4% sample rate, then that smaller partition size
	// cannot be guaranteed if docs are very large, and the $bucketAuto stage will return partitions that
	// average 400 MB anyway in such cases. In other cases where doc size limits are not reached, a
	// partition size under 400 MB would be honored.
	//
	// So 400 MB happens to be the lower bound on the partition sizes for a 4% sample rate, and also a
	// sensible partition size that's not too small or too large. This gives a reasonable expectation
	// for how large partitions will be, regardless of average document size.
	//
	defaultPartitionSizeInBytes = 400 * 1024 * 1024 // = 400 MB
)

// Replicator contains the id of a mongosync replicator.
// It is used here to avoid changing the interface of partitioning (from the mongosync version)
// overmuch.
type Replicator struct {
	ID string `bson:"id"`
}

// Partitions is a slice of partitions.
type Partitions struct {
	logger     *logger.Logger
	partitions []*Partition
}

// NewPartitions returns an empty partition slice.
func NewPartitions(logger *logger.Logger) *Partitions {
	return &Partitions{logger: logger, partitions: nil}
}

// AppendPartitions appends the input slice to the in-memory partitions.
func (p *Partitions) AppendPartitions(partitions []*Partition) {
	p.partitions = append(p.partitions, partitions...)
}

// GetSlice returns the slice of partitions.
func (p *Partitions) GetSlice() []*Partition {
	return p.partitions
}

type minOrMaxBound string

const (
	minBound minOrMaxBound = "min"
	maxBound minOrMaxBound = "max"
)

// PartitionCollectionWithSize splits the source collection into one or more
// partitions. These partitions are expected to be somewhat similar in size,
// but this is never guaranteed. The caller can choose a desired partition
// size, but not the internal parameters.
//
// For example, if we split a collection of documents with _id values from 1 to
// 100 into 4 partitions, then the partitions may look something like [1, 17],
// [17, 39], [39, 78], [78, 100]. For smaller collections resulting in only one
// partition, the partition will be [1, 100].
//
// A partition size of 0 means to use the default.
//
// Since these are useful elsewhere, this function also returns the
// collection’s document count and size (in bytes).
func PartitionCollectionWithSize(
	ctx context.Context,
	uuidEntry *uuidutil.NamespaceAndUUID,
	srcClient *mongo.Client,
	replicatorList []Replicator,
	subLogger *logger.Logger,
	partitionSizeInBytes int64,
	globalFilter map[string]any,
) ([]*Partition, types.DocumentCount, types.ByteCount, error) {
	if partitionSizeInBytes < 0 {
		subLogger.Warn().
			Int64("partitionSizeInBytes", partitionSizeInBytes).
			Int64("default", defaultPartitionSizeInBytes).
			Msg("Partition size is not valid; using default.")
	}
	if partitionSizeInBytes <= 0 {
		partitionSizeInBytes = defaultPartitionSizeInBytes
	}

	partitions, docCount, byteCount, err := PartitionCollectionWithParameters(
		ctx,
		uuidEntry,
		srcClient,
		replicatorList,
		defaultSampleRate,
		defaultSampleMinNumDocs,
		partitionSizeInBytes,
		subLogger,
		globalFilter,
	)

	// Handle timeout errors by partitioning without filtering.
	if mongo.IsTimeout(err) {
		subLogger.Debug().
			Err(err).
			Str("filter", fmt.Sprintf("%+v", globalFilter)).
			Msg("Timed out while partitioning with filter. Continuing by partitioning without the filter.")

		return PartitionCollectionWithParameters(
			ctx,
			uuidEntry,
			srcClient,
			replicatorList,
			defaultSampleRate,
			defaultSampleMinNumDocs,
			partitionSizeInBytes,
			subLogger,
			nil,
		)
	}

	return partitions, docCount, byteCount, err
}

// PartitionCollectionWithParameters is the implementation for
// PartitionCollection. It is only directly used in integration tests.
// See PartitionCollectionWithParameters for a description of inputs
// & outputs. (Alas, the parameter order differs slightly here …)
func PartitionCollectionWithParameters(
	ctx context.Context,
	uuidEntry *uuidutil.NamespaceAndUUID,
	srcClient *mongo.Client,
	replicatorList []Replicator,
	sampleRate float64,
	sampleMinNumDocs int,
	partitionSizeInBytes int64,
	subLogger *logger.Logger,
	globalFilter map[string]any,
) ([]*Partition, types.DocumentCount, types.ByteCount, error) {
	subLogger.Debug().
		Str("namespace", uuidEntry.DBName+"."+uuidEntry.CollName).
		Float64("sampleRate", sampleRate).
		Int("sampleMinNumDocs", sampleMinNumDocs).
		Int64("desiredPartitionSizeInBytes", partitionSizeInBytes).
		Msg("Partitioning collection.")

	// Get the source collection.
	srcDB := srcClient.Database(uuidEntry.DBName)
	srcColl := srcDB.Collection(uuidEntry.CollName)

	// Get the collection's size in bytes and its document count. It is okay if these return zero since there might still be
	// items in the collection. Rely on getOuterIDBound to do a majority read to determine if we continue processing the collection.
	collSizeInBytes, collDocCount, isCapped, err := GetSizeAndDocumentCount(ctx, subLogger, srcColl)
	if err != nil {
		return nil, 0, 0, err
	}

	// The lower bound for the collection. There is no partitioning to do if the bound is nil.
	minIDBound, err := getOuterIDBound(ctx, subLogger, minBound, srcDB, uuidEntry.CollName, globalFilter)
	if err != nil {
		return nil, 0, 0, err
	}
	if minIDBound == nil {
		subLogger.Info().
			Str("namespace", uuidEntry.DBName+"."+uuidEntry.CollName).
			Msg("No minimum _id found for collection; will not perform collection copy for this collection.")

		return nil, 0, 0, nil
	}

	// The upper bound for the collection. There is no partitioning to do if the bound is nil.
	maxIDBound, err := getOuterIDBound(ctx, subLogger, maxBound, srcDB, uuidEntry.CollName, globalFilter)
	if err != nil {
		return nil, 0, 0, err
	}
	if maxIDBound == nil {
		subLogger.Info().
			Str("namespace", uuidEntry.DBName+"."+uuidEntry.CollName).
			Msg("No maximum _id found for collection; will not perform collection copy for this collection.")

		return nil, 0, 0, nil
	}

	// The total number of partitions needed for the collection. If it is a capped collection, we
	// must only create one partition for the entire collection. Otherwise, calculate the
	// appropriate number of partitions.
	numPartitions := 1
	if !isCapped {
		// By default, number of partitions is calculated without considering the ratio of filtered documents.
		numPartitions = getNumPartitions(collSizeInBytes, partitionSizeInBytes, 1)

		// If a filter is used for partitioning, number of partitions is calculated with the ratio of filtered documents.
		if len(globalFilter) > 0 {
			numFilteredDocs, filteredCntErr := GetDocumentCountAfterFiltering(ctx, subLogger, srcColl, globalFilter)
			if filteredCntErr == nil {
				numPartitions = getNumPartitions(collSizeInBytes, partitionSizeInBytes, float64(numFilteredDocs)/float64(collDocCount))
			} else {
				return nil, 0, 0, filteredCntErr
			}
		}
	}

	// Prepend the lower bound and append the upper bound to any intermediate bounds.
	allIDBounds := make([]any, 0, numPartitions+1)
	allIDBounds = append(allIDBounds, minIDBound)

	// The intermediate bounds for the collection (i.e. all bounds apart from the lower and upper bounds).
	// It's okay for these bounds to be nil, since we already have the lower and upper bounds from which
	// to make at least one partition.
	var (
		midIDBounds   []any
		collDropped   bool
		prevSampleErr error
	)
	err = retry.New().
		WithErrorCodes(util.SampleTooManyDuplicatesErrCode).
		WithCallback(func(ctx context.Context, info *retry.FuncInfo) error {
			if info.GetAttemptNumber() > 0 && mmongo.ErrorHasCode(prevSampleErr, util.SampleTooManyDuplicatesErrCode) {
				subLogger.Debug().
					Err(prevSampleErr).
					Int("prevNumPartitions", numPartitions).
					Int("newNumPartitions", numPartitions).
					Msg("Retrying with decreased number of partitions. This will hopefully make $sample succeed.")

				numPartitions = numPartitions / 10
			}
			midIDBounds, collDropped, err = getMidIDBounds(
				ctx,
				subLogger,
				srcDB,
				uuidEntry.CollName,
				collDocCount,
				numPartitions,
				sampleMinNumDocs,
				sampleRate,
				globalFilter,
			)
			prevSampleErr = err
			return err
		}, "sampling documents to get partition mid bounds").Run(ctx, subLogger)

	if err != nil {
		return nil, 0, 0, err
	}
	if collDropped {
		// Skip this collection.
		return nil, 0, 0, nil
	}
	if midIDBounds != nil {
		allIDBounds = append(allIDBounds, midIDBounds...)
	}

	allIDBounds = append(allIDBounds, maxIDBound)

	if len(allIDBounds) < 2 {
		return nil, 0, 0, errors.Errorf("need at least 2 _id bounds to make a partition, but got %d _id bound(s)", len(allIDBounds))
	}

	// TODO (REP-552): Figure out what situations this occurs for, and whether or not it results from a bug.
	if len(allIDBounds) != numPartitions+1 {
		subLogger.Info().
			Int("idBounds", len(allIDBounds)).
			Int("numPartitions", numPartitions).
			Msg("_id bounds should outnumber partitions by 1.")
	}

	// Choose a random index to start to avoid over-assigning partitions to a specific replicator.
	// rand.Int() generates non-negative integers only.
	replIndex := rand.Int() % len(replicatorList)
	subLogger.Debug().
		Int("numPartitions", len(allIDBounds)-1).
		Str("namespace", uuidEntry.DBName+"."+uuidEntry.CollName).
		Bool("isCapped", isCapped).
		Msg("Creating partitions.")

	// Create the partitions with the index key bounds.
	partitions := make([]*Partition, 0, len(allIDBounds)-1)

	for i := 0; i < len(allIDBounds)-1; i++ {
		partitionKey := PartitionKey{
			SourceUUID:  uuidEntry.UUID,
			MongosyncID: replicatorList[replIndex].ID,
			Lower:       allIDBounds[i],
		}
		partition := &Partition{
			Key:      partitionKey,
			Ns:       &Namespace{uuidEntry.DBName, uuidEntry.CollName},
			Upper:    allIDBounds[i+1],
			IsCapped: isCapped,
		}
		partitions = append(partitions, partition)

		replIndex = (replIndex + 1) % len(replicatorList)
	}

	return partitions, types.DocumentCount(collDocCount), types.ByteCount(collSizeInBytes), nil
}

// GetSizeAndDocumentCount uses collStats to return a collection's byte size, document count, and
// capped status, in that order.
//
// Exported for usage in integration tests.
func GetSizeAndDocumentCount(ctx context.Context, logger *logger.Logger, srcColl *mongo.Collection) (int64, int64, bool, error) {
	srcDB := srcColl.Database()
	collName := srcColl.Name()

	value := struct {
		Size   int64 `bson:"size"`
		Count  int64 `bson:"count"`
		Capped bool  `bson:"capped"`
	}{}

	err := retry.New().WithCallback(
		func(ctx context.Context, ri *retry.FuncInfo) error {
			ri.Log(logger.Logger, "collStats", "source", srcDB.Name(), collName, "Retrieving collection size and document count.")
			request := bson.D{
				{"aggregate", collName},
				{"pipeline", mongo.Pipeline{
					{{"$collStats", bson.D{
						{"storageStats", bson.E{"scale", 1}},
					}}},
					// The "$group" here behaves as a project and rename when there's only one
					// document (non-sharded case).  When there are multiple documents (one for
					// each shard) it correctly sums the counts and sizes from each shard.
					{{"$group", bson.D{
						{"_id", "ns"},
						{"count", bson.D{{"$sum", "$storageStats.count"}}},
						{"size", bson.D{{"$sum", "$storageStats.size"}}},
						{"capped", bson.D{{"$first", "$capped"}}}}}},
				}},
				{"cursor", bson.D{}},
			}

			cursor, driverErr := srcDB.RunCommandCursor(ctx, request)
			if driverErr != nil {
				return driverErr
			}

			defer cursor.Close(ctx)
			if cursor.Next(ctx) {
				if err := cursor.Decode(&value); err != nil {
					return errors.Wrapf(err, "failed to decode $collStats response for source namespace %s.%s", srcDB.Name(), collName)
				}
			}
			return nil
		},
		"retrieving %#q's statistics",
		srcDB.Name()+"."+collName,
	).Run(ctx, logger)

	// TODO (REP-960): remove this check.
	// If we get NamespaceNotFoundError then return 0,0 since we won't do any partitioning with those returns
	// and the aggregation did not fail so we do not want to return an error. A
	// NamespaceNotFoundError can happen if the database does not exist.
	if util.IsNamespaceNotFoundError(err) {
		return 0, 0, false, nil
	}

	if err != nil {
		return 0, 0, false, errors.Wrapf(err, "failed to run aggregation for $collStats for source namespace %s.%s", srcDB.Name(), collName)
	}

	logger.Debug().
		Str("namespace", srcDB.Name()+"."+collName).
		Str("size", reportutils.FmtBytes(value.Size)).
		Int64("sizeInBytes", value.Size).
		Int64("docsCount", value.Count).
		Bool("isCapped", value.Capped).
		Msg("Collection stats.")

	return value.Size, value.Count, value.Capped, nil
}

// GetDocumentCountAfterFiltering counts the number of filtered documents in a collection.
//
// This function could take a long time, especially if the collection does not have an index
// on the filtered fields.
func GetDocumentCountAfterFiltering(ctx context.Context, logger *logger.Logger, srcColl *mongo.Collection, filter map[string]any) (int64, error) {
	srcDB := srcColl.Database()
	collName := srcColl.Name()

	value := struct {
		Count int64 `bson:"numFilteredDocs"`
	}{}

	var pipeline mongo.Pipeline

	if len(filter) > 0 {
		pipeline = append(pipeline, bson.D{{"$match", filter}})
	}
	pipeline = append(pipeline, bson.D{{"$count", "numFilteredDocs"}})

	err := retry.New().WithCallback(
		func(ctx context.Context, ri *retry.FuncInfo) error {
			ri.Log(logger.Logger, "count", "source", srcDB.Name(), collName, "Counting filtered documents.")
			request := bson.D{
				{"aggregate", collName},
				{"pipeline", pipeline},
				{"cursor", bson.D{}},
			}

			cursor, driverErr := srcDB.RunCommandCursor(ctx, request)
			if driverErr != nil {
				return driverErr
			}

			defer cursor.Close(ctx)
			if cursor.Next(ctx) {
				if err := cursor.Decode(&value); err != nil {
					return errors.Wrapf(err, "failed to decode $count response (%+v) for source namespace %s.%s after filter (%+v)", cursor.Current, srcDB.Name(), collName, filter)
				}
			}
			return nil
		},
		"counting %#q's filtered documents",
		srcDB.Name()+"."+collName,
	).Run(ctx, logger)

	// TODO (REP-960): remove this check.
	// If we get NamespaceNotFoundError then return 0 since we won't do any partitioning with those returns
	// and the aggregation did not fail so we do not want to return an error. A
	// NamespaceNotFoundError can happen if the database does not exist.
	if util.IsNamespaceNotFoundError(err) {
		return 0, nil
	}

	if err != nil {
		return 0, errors.Wrapf(err, "failed to run aggregation $count for source namespace %s.%s after filter (%+v)", srcDB.Name(), collName, filter)
	}

	logger.Debug().
		Str("namespace", srcDB.Name()+"."+collName).
		Int64("docsCount", value.Count).
		Str("filter", fmt.Sprintf("%+v", filter)).
		Msg("Collection stats.")

	return value.Count, nil
}

// getNumPartitions returns the total number of partitions needed for the collection,
// which is proportional to the percentage of filtered documents in the collection.
//
// The returned number is always 1 or greater, where 1 indicates that the collection
// can be represented with 1 partition and no additional splitting is needed.
func getNumPartitions(collSizeInBytes, partitionSizeInBytes int64, filteredRatio float64) int {
	// Get the number of partitions as a float.
	numPartitions := float64(collSizeInBytes) * filteredRatio / float64(partitionSizeInBytes)

	// We take the ceiling of the numPartitions needed, in order to honor the defaultPartitionSizeInBytes.
	//
	// E.g. if our collection is 1000 MB and the defaultPartitionSizeInBytes is 400 MB,
	// we'd need 1000 MB / 400 MB = 2.5 partitions for it. If we take the floor, we'd
	// end up with 1000 MB / 2 partitions = 500 MB / partition. But if we instead take
	// the ceiling, we'd end up with 1000 MB / 3 partitions = 333 MB / partition.
	return int(numPartitions) + 1
}

// getOuterIDBound returns either the smallest or largest _id value in a collection. The minOrMaxBound parameter can be set to "min" or "max" to get either, respectively.
// If a globalFilter is specified, getOuterIDBound returns the smallest or largest _id of documents within the filter.
func getOuterIDBound(
	ctx context.Context,
	subLogger *logger.Logger,
	minOrMaxBound minOrMaxBound,
	srcDB *mongo.Database,
	collName string,
	globalFilter map[string]any,
) (any, error) {
	// Choose a sort direction based on the minOrMaxBound.
	var sortDirection int
	switch minOrMaxBound {
	case minBound:
		sortDirection = 1
	case maxBound:
		sortDirection = -1
	default:
		return nil, errors.Errorf("unknown minOrMaxBound parameter '%v' when getting outer _id bound", minOrMaxBound)
	}

	var docID any

	var pipeline mongo.Pipeline
	if len(globalFilter) > 0 {
		pipeline = append(pipeline, bson.D{{"$match", globalFilter}})
	}
	pipeline = append(pipeline, []bson.D{
		{{"$sort", bson.D{{"_id", sortDirection}}}},
		{{"$project", bson.D{{"_id", 1}}}},
		{{"$limit", 1}},
	}...)

	// Get one document containing only the smallest or largest _id value in the collection.
	err := retry.New().WithCallback(
		func(ctx context.Context, ri *retry.FuncInfo) error {
			ri.Log(subLogger.Logger, "aggregate", "source", srcDB.Name(), collName, fmt.Sprintf("getting %s _id partition bound", minOrMaxBound))
			cursor, cmdErr :=
				srcDB.RunCommandCursor(ctx, bson.D{
					{"aggregate", collName},
					{"pipeline", pipeline},
					{"hint", bson.D{{"_id", 1}}},
					{"cursor", bson.D{}},
				})

			if cmdErr != nil {
				return cmdErr
			}

			// If we don't have at least one document, the collection is either empty or was dropped.
			defer cursor.Close(ctx)
			if !cursor.Next(ctx) {
				return nil
			}

			// Return the _id value from that document.
			docID, cmdErr = cursor.Current.LookupErr("_id")
			return cmdErr
		},
		"finding %#q's %s _id",
		srcDB.Name()+"."+collName,
		minOrMaxBound,
	).Run(ctx, subLogger)

	if err != nil {
		return nil, errors.Wrapf(err, "could not get %s _id bound for source collection '%s.%s'", minOrMaxBound, srcDB.Name(), collName)
	}

	return docID, nil

}

// getMidIDBounds performs a $sample and $bucketAuto aggregation on a collection and returns a slice of pseudo-randomly spaced out _id bounds.
// The number of bounds returned is: numPartitions - 1.
//
// A nil slice is returned if the collDocCount doesn't meet the sampleMinNumDocs, or if the numPartitions is less than 2.
func getMidIDBounds(
	ctx context.Context,
	logger *logger.Logger,
	srcDB *mongo.Database,
	collName string,
	collDocCount int64,
	numPartitions, sampleMinNumDocs int,
	sampleRate float64,
	globalFilter map[string]any,
) ([]any, bool, error) {
	// We entirely avoid sampling for mid bounds if we don't meet the criteria for the number of documents or partitions.
	if collDocCount < int64(sampleMinNumDocs) || numPartitions < 2 {
		return nil, false, nil
	}

	// We sample the lesser of 4% of a collection, or 3x the number of partitions.
	// See the constant definitions at the top of this file for rationale.
	numDocsToSample := int64(sampleRate * float64(collDocCount))
	if numDocsToSample > int64(defaultMaxNumDocsToSamplePerPartition*numPartitions) {
		numDocsToSample = int64(defaultMaxNumDocsToSamplePerPartition * numPartitions)
	}

	// INTEGRATION TEST ONLY. We sample all docs in a collection
	// to perform a collection scan and get deterministic results.
	if sampleRate == 1.0 {
		numDocsToSample = collDocCount
	}

	var pipeline mongo.Pipeline

	logEvent := logger.Info().
		Str("namespace", srcDB.Name()+"."+collName)

	if len(globalFilter) > 0 {
		pipeline = append(pipeline, bson.D{{"$match", globalFilter}})
		logEvent.Any("filter", globalFilter)
	} else {
		logEvent.Int("numDocsToSample", int(numDocsToSample))
	}

	logEvent.Msg("Sampling documents.")

	pipeline = append(pipeline, []bson.D{
		{{"$sample", bson.D{{"size", numDocsToSample}}}},
		{{"$project", bson.D{{"_id", 1}}}},
		{{"$bucketAuto",
			bson.D{
				{"groupBy", "$_id"},
				{"buckets", numPartitions},
			}}},
	}...)

	// Get a cursor for the $sample and $bucketAuto aggregation.
	var midIDBounds []any
	agRetryer := retry.New()
	err := agRetryer.
		WithCallback(
			func(ctx context.Context, ri *retry.FuncInfo) error {
				ri.Log(logger.Logger, "aggregate", "source", srcDB.Name(), collName, "Retrieving mid _id partition bounds using $sample.")
				cursor, cmdErr :=
					srcDB.RunCommandCursor(ctx, bson.D{
						{"aggregate", collName},
						{"pipeline", pipeline},
						{"allowDiskUse", true},
						{"cursor", bson.D{}},
					})

				if cmdErr != nil {
					return errors.Wrapf(cmdErr, "failed to $sample and $bucketAuto documents for source namespace '%s.%s'", srcDB.Name(), collName)
				}

				defer cursor.Close(ctx)

				// Iterate through all $bucketAuto documents of the form:
				// {
				//   "_id" : {
				//     "min" : ... ,
				//     "max" : ...
				//   },
				//   "count" : ...
				// }
				midIDBounds = make([]any, 0, numPartitions)
				for cursor.Next(ctx) {
					// Get a mid _id bound using the $bucketAuto document's max value.
					bucketAutoDoc := make(bson.Raw, len(cursor.Current))
					copy(bucketAutoDoc, cursor.Current)
					bound, err := bucketAutoDoc.LookupErr("_id", "max")
					if err != nil {
						return errors.Wrapf(err, "failed to lookup '_id.max' key in $bucketAuto document for source namespace '%s.%s'", srcDB.Name(), collName)
					}

					// Append the copied bound to the other mid _id bounds.
					midIDBounds = append(midIDBounds, bound)
					ri.NoteSuccess("received an ID partition")
				}

				return cursor.Err()
			},
			"finding %#q's _id partition boundaries",
			srcDB.Name()+"."+collName,
		).Run(ctx, logger)

	if err != nil {
		return nil, false, errors.Wrapf(err, "encountered a problem in the cursor when trying to $sample and $bucketAuto aggregation for source namespace '%s.%s'", srcDB.Name(), collName)
	}

	if len(midIDBounds) == 0 {
		return nil, false, nil
	}

	// We remove the last $bucketAuto max value, since it does not qualify as a mid-bound.
	return midIDBounds[:len(midIDBounds)-1], false, nil
}
