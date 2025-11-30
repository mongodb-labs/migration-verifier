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
	"golang.org/x/sync/errgroup"
)

type ddlEventHandling string

const (
	fauxDocSizeForDeleteEvents = 1024

	// The number of batches we’ll hold in memory at once.
	batchChanBufferSize = 100

	onDDLEventAllow ddlEventHandling = "allow"

	changeReaderCollectionName = "changeReader"
)

type readerCurrentTimes struct {
	LastResumeTime  bson.Timestamp
	LastClusterTime bson.Timestamp
}

func (rp readerCurrentTimes) Lag() time.Duration {
	return time.Second * time.Duration(
		int(rp.LastClusterTime.T)-int(rp.LastResumeTime.T),
	)
}

type changeReader interface {
	getWhichCluster() whichCluster
	getReadChannel() <-chan changeEventBatch
	getStartTimestamp() bson.Timestamp
	getLastSeenClusterTime() option.Option[bson.Timestamp]
	getEventsPerSecond() option.Option[float64]
	getCurrentTimes() option.Option[readerCurrentTimes]
	getBufferSaturation() float64
	setWritesOff(bson.Timestamp)
	start(context.Context, *errgroup.Group) error
	persistResumeToken(context.Context, bson.Raw) error
	isRunning() bool
	String() string
}

type ChangeReaderCommon struct {
	readerType whichCluster

	logger     *logger.Logger
	namespaces []string

	metaDB        *mongo.Database
	watcherClient *mongo.Client
	clusterInfo   util.ClusterInfo

	resumeTokenTSExtractor func(bson.Raw) (bson.Timestamp, error)

	running              bool
	changeEventBatchChan chan changeEventBatch
	writesOffTs          *util.Eventual[bson.Timestamp]

	lastChangeEventTime *msync.TypedAtomic[option.Option[bson.Timestamp]]

	currentTimes *msync.TypedAtomic[option.Option[readerCurrentTimes]]

	startAtTs *bson.Timestamp

	batchSizeHistory *history.History[int]

	onDDLEvent ddlEventHandling
}

func newChangeReaderCommon(clusterName whichCluster) ChangeReaderCommon {
	return ChangeReaderCommon{
		readerType:           clusterName,
		changeEventBatchChan: make(chan changeEventBatch, batchChanBufferSize),
		writesOffTs:          util.NewEventual[bson.Timestamp](),
		//lag:                  msync.NewTypedAtomic(option.None[time.Duration]()),
		lastChangeEventTime: msync.NewTypedAtomic(option.None[bson.Timestamp]()),
		batchSizeHistory:    history.New[int](time.Minute),
		onDDLEvent: lo.Ternary(
			clusterName == dst,
			onDDLEventAllow,
			"",
		),
	}
}

func (rc *ChangeReaderCommon) getWhichCluster() whichCluster {
	return rc.readerType
}

func (rc *ChangeReaderCommon) getStartTimestamp() bson.Timestamp {
	if rc.startAtTs == nil {
		panic("no start timestamp yet?!?")
	}

	return *rc.startAtTs
}

func (rc *ChangeReaderCommon) setWritesOff(ts bson.Timestamp) {
	rc.writesOffTs.Set(ts)
}

func (rc *ChangeReaderCommon) isRunning() bool {
	return rc.running
}

func (rc *ChangeReaderCommon) getReadChannel() <-chan changeEventBatch {
	return rc.changeEventBatchChan
}

func (rc *ChangeReaderCommon) getLastSeenClusterTime() option.Option[bson.Timestamp] {
	return rc.lastChangeEventTime.Load()
}

// getBufferSaturation returns the reader’s internal buffer’s saturation level
// as a fraction. If saturation rises, that means we’re reading events faster
// than we can persist them.
func (rc *ChangeReaderCommon) getBufferSaturation() float64 {
	return util.DivideToF64(len(rc.changeEventBatchChan), cap(rc.changeEventBatchChan))
}

func (rc *ChangeReaderCommon) getCurrentTimes() option.Option[readerCurrentTimes] {
	return rc.currentTimes.Load()
}

/*
// getLag returns the observed change stream lag (i.e., the delta between
// cluster time and the most-recently-seen change event).
func (rc *ChangeReaderCommon) getLag() option.Option[time.Duration] {
	if prog, has := rc.progress.Load().Get(); has {
		return option.Some(
			time.Duration(int(prog.lastClusterTime.T)-int(prog.lastResumeTime.T)) * time.Second,
		)
	}

	return option.None[time.Duration]()
}
*/

// getEventsPerSecond returns the number of change events per second we’ve been
// seeing “recently”. (See implementation for the actual period over which we
// compile this metric.)
func (rc *ChangeReaderCommon) getEventsPerSecond() option.Option[float64] {
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

func (rc *ChangeReaderCommon) persistResumeToken(ctx context.Context, token bson.Raw) error {
	coll := rc.metaDB.Collection(changeReaderCollectionName)
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

		logEvent.Msgf("Persisted %s's resume token.", rc.readerType)

		return nil
	}

	return errors.Wrapf(err, "failed to persist %s resume token (%v)", rc.readerType, token)
}

func (rc *ChangeReaderCommon) resumeTokenDocID() string {
	switch rc.readerType {
	case src:
		return "srcResumeToken"
	case dst:
		return "dstResumeToken"
	default:
		panic("unknown readerType: " + rc.readerType)
	}
}

func (rc *ChangeReaderCommon) getMetadataCollection() *mongo.Collection {
	return rc.metaDB.Collection(changeReaderCollectionName)
}

func (rc *ChangeReaderCommon) loadResumeToken(ctx context.Context) (option.Option[bson.Raw], error) {
	coll := rc.getMetadataCollection()

	token, err := coll.FindOne(
		ctx,
		bson.D{{"_id", rc.resumeTokenDocID()}},
	).Raw()

	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			err = nil
		}

		return option.None[bson.Raw](), err
	}

	return option.Some(token), nil
}

func (rc *ChangeReaderCommon) updateLag(sess *mongo.Session, token bson.Raw) {
	tokenTs, err := rc.resumeTokenTSExtractor(token)
	if err == nil {
		cTime, err := util.GetClusterTimeFromSession(sess)
		if err != nil {
			rc.logger.Warn().
				Err(err).
				Str("reader", string(rc.getWhichCluster())).
				Msg("Failed to extract cluster time from session.")
		} else {
			rc.currentTimes.Store(option.Some(readerCurrentTimes{
				LastResumeTime:  tokenTs,
				LastClusterTime: cTime,
			}))
		}
	} else {
		rc.logger.Warn().
			Err(err).
			Msgf("Failed to extract timestamp from %s's resume token to compute lag.", rc.readerType)
	}
}

func (rc *ChangeReaderCommon) logIgnoredDDL(rawEvent bson.Raw) {
	rc.logger.Info().
		Str("reader", string(rc.readerType)).
		Stringer("event", rawEvent).
		Msg("Ignoring event with unrecognized type on destination. (It’s assumedly internal to the migration.)")
}

func addTimestampToLogEvent(ts bson.Timestamp, event *zerolog.Event) *zerolog.Event {
	return event.
		Any("timestamp", ts).
		Time("time", time.Unix(int64(ts.T), int64(0)))
}
