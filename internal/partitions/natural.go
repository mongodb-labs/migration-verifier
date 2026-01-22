package partitions

import (
	"context"
	"fmt"

	"github.com/10gen/migration-verifier/chanutil"
	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/mmongo/cursor"
	"github.com/10gen/migration-verifier/option"
	"github.com/mongodb-labs/migration-tools/bsontools"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/samber/mo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
)

const (
	// RecordID is the server’s name for record IDs in responses.
	RecordID = "$recordId"
)

// PartitionCollectionNaturalOrder spawns a goroutine that partitions the
// collection in natural order.
//
// NB: This ignores document filtering because we’re doing a collection
// scan anyway later on to compare the documents.
func PartitionCollectionNaturalOrder(
	ctx context.Context,
	coll *mongo.Collection,
	idealPartitionBytes types.ByteCount,
	subLogger *logger.Logger,
) (chan mo.Result[Partition], error) {
	pChan := make(chan mo.Result[Partition])

	// Avoid storing a null upper limit. See architecture
	// documentation for rationale.
	topRecordIDOpt, err := GetTopRecordID(ctx, coll)
	if err != nil {
		return nil, errors.Wrapf(err, "fetching top record ID")
	}

	if !topRecordIDOpt.IsSome() {
		// If the collection is empty then there’s no point in partitioning it.
		// Any documents created during generation 0 will be rechecked in
		// generation 1.
		close(pChan)

		return pChan, nil
	}

	collSizeInBytes, docCount, _, err := GetSizeAndDocumentCount(ctx, subLogger, coll)
	if err != nil {
		return nil, errors.Wrapf(err, "getting collection size & count")
	}

	if docCount == 0 {
		// Again: if the collection is empty, there’s no point in partitioning.
		close(pChan)

		return pChan, nil
	}

	idealNumPartitions := util.DivideToF64(collSizeInBytes, idealPartitionBytes)

	// Use min() to prevent docCount==0 or some other silliness from
	// causing a failure.
	sampleRate := min(1, util.DivideToF64(idealNumPartitions, docCount))

	cmd := bson.D{
		{"find", coll.Name()},
		{"hint", bson.D{{"$natural", 1}}},

		// We fetch partition boundaries one at a time.
		{"batchSize", 1},
		{"filter", bson.D{{"$sampleRate", sampleRate}}},
		{"$_requestResumeToken", true},
		{"readConcern", bson.D{
			{"level", "majority"},
		}},

		// Discard the actual document. All we want are the resume tokens.
		{"projection", bson.D{
			{"_id", 0},
			{"_", bson.D{{"$literal", true}}},
		}},

		{"comment", "partition"},
	}

	sess, err := coll.Database().Client().StartSession()
	if err != nil {
		return nil, errors.Wrapf(err, "starting session")
	}

	sessCtx := mongo.NewSessionContext(ctx, sess)

	resp := coll.Database().RunCommand(sessCtx, cmd)

	c, err := cursor.New(coll.Database(), resp, sess)
	if err != nil {
		return nil, errors.Wrapf(err, "opening partition query (%+v)", cmd)
	}

	curTokenOpt, err := cursor.GetResumeToken(c)
	if err != nil {
		return nil, errors.Wrapf(err, "extracting resume token")
	}

	// Confirm that we can, in fact, partition this collection naturally:
	if curToken, hasToken := curTokenOpt.Get(); hasToken {
		recIDRV, err := curToken.LookupErr(RecordID)
		if err != nil {
			return nil, errors.Wrapf(err, "extracting record ID from resume token (%v)", curToken)
		}

		switch recIDRV.Type {
		case bson.TypeInt64, bson.TypeBinary:
			// All good! We recognize these record ID types.
		default:
			// This likely indicates a new, unexpected collection type.
			return nil, fmt.Errorf("unknown BSON type (%s) for record ID (%s)", recIDRV.Type, recIDRV)
		}
	}

	// Partitions by record ID are only meaningful on a single mongod.
	// (i.e., 2 nodes within the same replica set will have different
	// record IDs for the same document)
	//
	// Thus, along with the resume tokens we also persist the hostname
	// and port.
	helloRaw, err := util.GetHelloRaw(ctx, coll.Database().Client())
	if err != nil {
		return nil, errors.Wrapf(err, "sending hello/isMaster")
	}

	hostnameAndPortRV, err := helloRaw.LookupErr("me")
	if err != nil {
		return nil, errors.Wrapf(err, "parsing isMaster")
	}

	hostnameAndPort, err := bsontools.RawValueTo[string](hostnameAndPortRV)
	if err != nil {
		return nil, errors.Wrapf(err, "parsing hostname in isMaster")
	}

	go func() {
		defer close(pChan)

		priorToken := bsontools.ToRawValue(bson.Null{})
		var err error

		for !c.IsFinished() {
			var curTokenOpt option.Option[bson.Raw]
			curTokenOpt, err = cursor.GetResumeToken(c)
			if err != nil {
				err = errors.Wrapf(err, "reading resume token from server response")
				break
			}

			curToken, hasToken := curTokenOpt.Get()
			if !hasToken {
				break
			}

			var recIDRV bson.RawValue
			recIDRV, err = curToken.LookupErr(RecordID)
			if err != nil {
				err = errors.Wrapf(err, "reading record ID from resume token (%v)", curToken)
				break
			}

			if recIDRV.Type == bson.TypeNull {
				recIDRV = topRecordIDOpt.MustGetf("got null resume token but have no top record ID?!?")
			}

			partition := Partition{
				Natural:         true,
				HostnameAndPort: option.Some(hostnameAndPort),
				Ns:              &Namespace{coll.Database().Name(), coll.Name()},
				Key: PartitionKey{
					Lower: priorToken,
				},
				Upper: recIDRV,
			}

			err = chanutil.WriteWithDoneCheck(
				ctx,
				pChan,
				mo.Ok(partition),
			)

			if err != nil && !errors.Is(err, context.Canceled) {
				subLogger.Warn().
					Err(err).
					Any("partition", partition).
					Msg("Failed to report new partition.")

				return
			}

			if c.IsFinished() {
				break
			}

			priorToken = bsontools.ToRawValue(curToken)

			err = c.GetNext(ctx, bson.E{"batchSize", 1})
			if err != nil {
				err = errors.Wrapf(err, "fetching next partition bound")
				break
			}
		}

		if err != nil {
			err := chanutil.WriteWithDoneCheck(
				ctx,
				pChan,
				mo.Err[Partition](err),
			)

			if err != nil && !errors.Is(err, context.Canceled) {
				subLogger.Warn().
					Err(err).
					AnErr("cursorErr", err).
					Msg("Failed to report cursor error.")
			}
		}
	}()

	return pChan, nil
}

func GetTopRecordID(
	ctx context.Context,
	coll *mongo.Collection,
) (option.Option[bson.RawValue], error) {
	cursor, err := coll.
		Database().
		Collection(coll.Name(), options.Collection().
			SetReadConcern(readconcern.Majority()),
		).
		Find(
			ctx,
			bson.D{},
			options.Find().
				SetSort(bson.D{{"$natural", -1}}).
				SetLimit(1).
				SetProjection(bson.D{{"id", 0}}).
				SetShowRecordID(true),
		)

	if err != nil {
		return option.None[bson.RawValue](), errors.Wrap(
			err,
			"fetching last record ID",
		)
	}

	var docs []bson.Raw
	err = cursor.All(ctx, &docs)
	if err != nil {
		return option.None[bson.RawValue](), errors.Wrap(
			err,
			"reading last record ID",
		)
	}

	lastDoc, ok := lo.Last(docs)

	if !ok {
		return option.None[bson.RawValue](), nil
	}

	recID, err := lastDoc.LookupErr(RecordID)

	return option.Some(recID), errors.Wrapf(err, "extracting record ID (doc: %v)", lastDoc)
}
