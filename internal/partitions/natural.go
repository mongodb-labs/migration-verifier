package partitions

import (
	"context"

	"github.com/10gen/migration-verifier/chanutil"
	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/mmongo/cursor"
	"github.com/mongodb-labs/migration-tools/bsontools"
	"github.com/pkg/errors"
	"github.com/samber/mo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// PartitionCollectionNaturalOrder spawns a goroutine that partitions the
// collection in natural order.
func PartitionCollectionNaturalOrder(
	ctx context.Context,
	coll *mongo.Collection,
	idealPartitionBytes types.ByteCount,
	subLogger *logger.Logger,
	globalFilter bson.D,
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

	pChan := make(chan mo.Result[Partition])

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
			if c.IsFinished() {
				recIDRV = bsontools.ToRawValue(bson.Null{})
			} else {
				recIDRV, err = curToken.LookupErr("$recordId")
				if err != nil {
					err = errors.Wrapf(err, "extracting record ID from resume token (%v)", curToken)
					break
				}
			}

			partition := Partition{
				Natural: true,
				Ns:      &Namespace{coll.Database().Name(), coll.Name()},
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
