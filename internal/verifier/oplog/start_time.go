package oplog

import (
	"context"
	"fmt"

	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
)

func GetTailingStartTime(
	ctx context.Context,
	client *mongo.Client,
) (OpTime, error) {
	oldestTxn, err := getOldestTransactionTime(ctx, client)
	if err != nil {
		return OpTime{}, errors.Wrapf(err, "finding oldest txn")
	}

	if oldestTime, has := oldestTxn.Get(); has {
		return oldestTime, nil
	}

	return getLatestVisibleOplogOpTime(ctx, client)
}

type OpTime struct {
	TS bson.Timestamp
	T  int64
	H  option.Option[int64]
}

func (ot OpTime) Equals(ot2 OpTime) bool {
	if !ot.TS.Equal(ot2.TS) {
		return false
	}

	if ot.T != ot2.T {
		return false
	}

	return ot.H.OrZero() == ot2.H.OrZero()
}

// GetLatestOplogOpTime returns the optime of the most recent oplog
// record satisfying the given `query` or a zero-value db.OpTime{} if
// no oplog record matches.  This method does not ensure that all prior oplog
// entries are visible (i.e. have been storage-committed).
func getLatestOplogOpTime(
	ctx context.Context,
	client *mongo.Client,
) (OpTime, error) {
	var optime OpTime

	opts := options.FindOne().
		SetProjection(bson.M{"ts": 1, "t": 1, "h": 1}).
		SetSort(bson.D{{"$natural", -1}})

	coll := client.Database("local").Collection("oplog.rs")

	res := coll.FindOne(ctx, bson.D{}, opts)
	if err := res.Err(); err != nil {
		return OpTime{}, err
	}

	if err := res.Decode(&optime); err != nil {
		return OpTime{}, err
	}
	return optime, nil
}

func getLatestVisibleOplogOpTime(
	ctx context.Context,
	client *mongo.Client,
) (OpTime, error) {

	latestOpTime, err := getLatestOplogOpTime(ctx, client)
	if err != nil {
		return OpTime{}, err
	}

	coll := client.Database("local").Collection("oplog.rs")

	// Do a forward scan starting at the last op fetched to ensure that
	// all operations with earlier oplog times have been storage-committed.
	result, err := coll.FindOne(ctx,
		bson.M{"ts": bson.M{"$gte": latestOpTime.TS}},
		options.FindOne().SetOplogReplay(true),
	).Raw()
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return OpTime{}, fmt.Errorf(
				"last op was not confirmed. last optime: %+v. confirmation time was not found",
				latestOpTime,
			)
		}
		return OpTime{}, err
	}

	var optime OpTime

	if err := bson.Unmarshal(result, &optime); err != nil {
		return OpTime{}, errors.Wrap(err, "local.oplog.rs error")
	}

	if !optime.Equals(latestOpTime) {
		return OpTime{}, fmt.Errorf(
			"last op was not confirmed. last optime: %+v. confirmation time: %+v",
			latestOpTime,
			optime,
		)
	}

	return latestOpTime, nil
}

func getOldestTransactionTime(
	ctx context.Context,
	client *mongo.Client,
) (option.Option[OpTime], error) {
	coll := client.Database("config").
		Collection(
			"transactions",
			options.Collection().SetReadConcern(readconcern.Local()),
		)

	decoded := struct {
		StartOpTime OpTime
	}{}

	err := coll.FindOne(
		ctx,
		bson.D{
			{"state", bson.D{
				{"$in", bson.A{"prepared", "inProgress"}},
			}},
		},
		options.FindOne().SetSort(bson.D{{"startOpTime", 1}}),
	).Decode(&decoded)

	if errors.Is(err, mongo.ErrNoDocuments) {
		return option.None[OpTime](), nil
	}

	if err != nil {
		return option.None[OpTime](), errors.Wrap(err, "config.transactions.findOne")
	}

	return option.Some(decoded.StartOpTime), nil
}
