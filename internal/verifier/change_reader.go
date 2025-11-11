package verifier

import (
	"context"
	"time"

	"github.com/10gen/migration-verifier/history"
	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/msync"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func addTimestampToLogEvent(ts bson.Timestamp, event *zerolog.Event) *zerolog.Event {
	return event.
		Any("timestamp", ts).
		Time("time", time.Unix(int64(ts.T), int64(0)))
}

func (rc ChangeReaderCommon) wrapPersistorErrorForReader() error {
	return errors.Wrap(
		rc.persistorError.Get(),
		"persistor failed, so no more events can be processed",
	)
}

type changeReader interface {
	getWhichCluster() whichCluster
	getReadChannel() <-chan changeEventBatch
	getError() *util.Eventual[error]
	getStartTimestamp() bson.Timestamp
	getEventsPerSecond() option.Option[float64]
	getLag() option.Option[time.Duration]
	getBufferSaturation() float64
	setWritesOff(bson.Timestamp)
	setPersistorError(error)
	start(context.Context) error
	done() <-chan struct{}
	persistChangeStreamResumeToken(context.Context, bson.Raw) error
	isRunning() bool
	String() string
}

type ChangeReaderCommon struct {
	logger     *logger.Logger
	namespaces []string

	client      *mongo.Client
	clusterInfo util.ClusterInfo

	metaDB *mongo.Database

	resumeTokenTSExtractor func(bson.Raw) (bson.Timestamp, error)

	running          bool
	clusterName      whichCluster
	doneChan         chan struct{}
	eventsChan       chan changeEventBatch
	startAtTS        option.Option[bson.Timestamp]
	readerError      *util.Eventual[error]
	persistorError   *util.Eventual[error]
	writesOffTS      *util.Eventual[bson.Timestamp]
	lag              *msync.TypedAtomic[option.Option[time.Duration]]
	batchSizeHistory *history.History[int]
	lastChangeTime   option.Option[bson.Timestamp]
}

func (rc ChangeReaderCommon) getWhichCluster() whichCluster {
	return rc.clusterName
}

func (rc ChangeReaderCommon) setPersistorError(err error) {
	rc.persistorError.Set(err)
}

func (rc ChangeReaderCommon) getError() *util.Eventual[error] {
	return rc.readerError
}

func (rc ChangeReaderCommon) getStartTimestamp() bson.Timestamp {
	return rc.startAtTS.MustGet()
}

func (rc ChangeReaderCommon) setWritesOff(ts bson.Timestamp) {
	rc.writesOffTS.Set(ts)
}

func (rc ChangeReaderCommon) isRunning() bool {
	return rc.running
}

func (rc ChangeReaderCommon) getReadChannel() <-chan changeEventBatch {
	return rc.eventsChan
}

func (rc ChangeReaderCommon) done() <-chan struct{} {
	return rc.doneChan
}

func (rc ChangeReaderCommon) getBufferSaturation() float64 {
	return util.DivideToF64(len(rc.eventsChan), cap(rc.eventsChan))
}

func (rc ChangeReaderCommon) getLag() option.Option[time.Duration] {
	return rc.lag.Load()
}

// getEventsPerSecond returns the number of change events per second we’ve been
// seeing “recently”. (See implementation for the actual period over which we
// compile this metric.)
func (rc ChangeReaderCommon) getEventsPerSecond() option.Option[float64] {
	logs := rc.batchSizeHistory.Get()
	lastLog, hasLogs := lo.Last(logs)

	if hasLogs && lastLog.At != logs[0].At {
		span := lastLog.At.Sub(logs[0].At)

		// Each log contains a time and a # of events that happened since
		// the prior log. Thus, each log’s Datum is a count of events that
		// happened before the timestamp. Since we want the # of events that
		// happened between the first & last times, we only want events *after*
		// the first time. Thus, we skip the first log entry here.
		totalEvents := 0
		for _, log := range logs[1:] {
			totalEvents += log.Datum
		}

		return option.Some(util.DivideToF64(totalEvents, span.Seconds()))
	}

	return option.None[float64]()
}

func (rc ChangeReaderCommon) persistChangeStreamResumeToken(ctx context.Context, token bson.Raw) error {
	coll := rc.metaDB.Collection(metadataChangeStreamCollectionName)
	_, err := coll.ReplaceOne(
		ctx,
		bson.D{{"_id", rc.resumeTokenDocID()}},
		token,
		options.Replace().SetUpsert(true),
	)

	if err == nil {
		ts, err := rc.resumeTokenTSExtractor(token)

		logEvent := rc.logger.Debug()

		if err == nil {
			logEvent = addTimestampToLogEvent(ts, logEvent)
		} else {
			rc.logger.Warn().Err(err).
				Msg("failed to extract resume token timestamp")
		}

		logEvent.Msgf("Persisted %s's resume token.", rc.clusterName)

		return nil
	}

	return errors.Wrapf(err, "failed to persist change stream resume token (%v)", token)
}

func (rc ChangeReaderCommon) resumeTokenDocID() string {
	switch rc.clusterName {
	case src:
		return "srcResumeToken"
	case dst:
		return "dstResumeToken"
	default:
		panic("unknown readerType: " + rc.clusterName)
	}
}

func (rc ChangeReaderCommon) getChangeStreamMetadataCollection() *mongo.Collection {
	return rc.metaDB.Collection(metadataChangeStreamCollectionName)
}

func (rc ChangeReaderCommon) loadChangeStreamResumeToken(ctx context.Context) (bson.Raw, error) {
	coll := rc.getChangeStreamMetadataCollection()

	token, err := coll.FindOne(
		ctx,
		bson.D{{"_id", rc.resumeTokenDocID()}},
	).Raw()

	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}

	return token, err
}

func (rc *ChangeReaderCommon) updateLag(sess *mongo.Session, token bson.Raw) {
	var tokenTs bson.Timestamp
	tokenTs, err := rc.resumeTokenTSExtractor(token)
	if err == nil {
		lagSecs := int64(sess.OperationTime().T) - int64(tokenTs.T)
		rc.lag.Store(option.Some(time.Second * time.Duration(lagSecs)))
	} else {
		rc.logger.Warn().
			Err(err).
			Msgf("Failed to extract timestamp from %s's resume token to compute lag.", rc.clusterName)
	}
}
