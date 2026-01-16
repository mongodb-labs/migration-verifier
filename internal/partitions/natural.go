package partitions

import (
	"context"
	"fmt"

	"github.com/10gen/migration-verifier/chanutil"
	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/mmongo"
	"github.com/10gen/migration-verifier/mmongo/cursor"
	"github.com/10gen/migration-verifier/option"
	"github.com/mongodb-labs/migration-tools/bsontools"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/samber/mo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// CannotResumeNaturalError indicates a collection that cannot be
// partitioned naturally with resumability. These can be safely handled
// in a single thread.
type CannotResumeNaturalError struct {
	ns  string
	err error
}

var _ error = CannotResumeNaturalError{}

func (c CannotResumeNaturalError) Error() string {
	return c.Cause().Error()
}

func (c CannotResumeNaturalError) Cause() error {
	return fmt.Errorf("cannot resume %#q natural partitions: %v", c.ns, c.err)
}

// CreateSingleNaturalOrderPartition returns a partition that indicates to
// scan the entire collection in a single thread. This is useful if the
// source cluster cannot partition a natural scan.
func CreateSingleNaturalOrderPartition(
	db, coll string,
) Partition {
	return Partition{
		Natural: true,
		Ns:      &Namespace{db, coll},
		Key: PartitionKey{
			Lower: bsontools.ToRawValue(bson.Null{}),
		},
		Upper: bsontools.ToRawValue(bson.Null{}),
	}
}

// PartitionCollectionNaturalOrder spawns a goroutine that partitions the
// collection in natural order.
//
// If the collection can’t be partitioned naturally, this returns a
// CannotPartitionNaturalError.
//
// NB: This ignores document filtering because we’re doing a collection
// scan anyway later on to compare the documents.
func PartitionCollectionNaturalOrder(
	ctx context.Context,
	coll *mongo.Collection,
	idealPartitionBytes types.ByteCount,
	subLogger *logger.Logger,
) (chan mo.Result[Partition], error) {

	collSizeInBytes, docCount, _, err := GetSizeAndDocumentCount(ctx, subLogger, coll)
	if err != nil {
		return nil, errors.Wrapf(err, "getting collection size & count")
	}

	idealNumPartitions := util.DivideToF64(collSizeInBytes, idealPartitionBytes)

	// Use min() to prevent docCount==0 or some other silliness from
	// causing a failure.
	sampleRate := min(1, util.DivideToF64(idealNumPartitions, docCount))

	cmd := bson.D{
		{"find", coll.Name()},
		{"hint", bson.D{{"$natural", 1}}},
		{"batchSize", 1},
		{"filter", bson.D{{"$sampleRate", sampleRate}}},
		{"$_requestResumeToken", true},

		// Discard the actual document. All we want are the resume tokens.
		{"projection", bson.D{
			{"_id", 0},
			{"_", bson.D{{"$literal", true}}},
		}},
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

	// Confirm that we can, in fact, partition this collection naturally:
	curToken, err := cursor.GetResumeToken(c)
	if err != nil {
		return nil, errors.Wrapf(err, "extracting resume token")
	}
	if !c.IsFinished() {
		recIDRV, err := curToken.LookupErr("$recordId")
		if err != nil {
			return nil, errors.Wrapf(err, "extracting record ID from resume token (%v)", curToken)
		}

		switch recIDRV.Type {
		case bson.TypeInt64:
			// A normal collection. All is well.
		case bson.TypeBinary:
			version, err := mmongo.GetVersionArray(ctx, coll.Database().Client())
			if err != nil {
				return nil, errors.Wrapf(err, "fetching cluster version")
			}

			if !mmongo.FindCanUseStartAt(version) {
				return nil, CannotResumeNaturalError{
					ns:  coll.Database().Name() + "." + coll.Name(),
					err: fmt.Errorf("verification of clustered collection requires a newer source version"),
				}
			}
		default:
			// This likely indicates a new, unexpected collection type.
			return nil, CannotResumeNaturalError{
				ns:  coll.Database().Name() + "." + coll.Name(),
				err: fmt.Errorf("unknown BSON type (%s) for record ID (%s)", recIDRV.Type, recIDRV),
			}
		}
	}

	helloRaw, err := util.GetHelloRaw(ctx, coll.Database().Client())
	if err != nil {
		return nil, errors.Wrapf(err, "sending hello/isMaster")
	}

	hostnameRV, err := helloRaw.LookupErr("me")
	if err != nil {
		return nil, errors.Wrapf(err, "parsing isMaster")
	}

	hostname, err := bsontools.RawValueTo[string](hostnameRV)
	if err != nil {
		return nil, errors.Wrapf(err, "parsing hostname in isMaster")
	}

	pChan := make(chan mo.Result[Partition])

	// Avoid storing a null upper limit if we can. See architecture
	// documentation for rationale.
	topRecordID, err := getTopRecordID(ctx, coll)
	if err != nil {
		return nil, errors.Wrapf(err, "fetching top record ID")
	}

	go func() {
		defer close(pChan)

		priorToken := bsontools.ToRawValue(bson.Null{})
		var curToken bson.Raw
		var err error

		for {
			curToken, err = cursor.GetResumeToken(c)
			if err != nil {
				err = errors.Wrapf(err, "extracting resume token")
				break
			}

			var recIDRV bson.RawValue
			recIDRV, err = curToken.LookupErr("$recordId")
			if err != nil {
				err = errors.Wrapf(err, "extracting record ID from resume token (%v)", curToken)
				break
			}

			if recIDRV.Type == bson.TypeNull {
				if topID, has := topRecordID.Get(); has {
					recIDRV = topID
				}
			}

			partition := Partition{
				Natural:  true,
				Hostname: option.Some(hostname),
				Ns:       &Namespace{coll.Database().Name(), coll.Name()},
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
				err = errors.Wrapf(err, "fetching next batch")
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

func getTopRecordID(
	ctx context.Context,
	coll *mongo.Collection,
) (option.Option[bson.RawValue], error) {
	cursor, err := coll.Find(
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

	recID, err := lastDoc.LookupErr("$recordId")

	return option.Some(recID), errors.Wrapf(err, "extracting record ID (doc: %v)", lastDoc)
}
