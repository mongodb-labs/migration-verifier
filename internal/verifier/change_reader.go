package verifier

import (
	"context"
	"time"

	"github.com/10gen/migration-verifier/history"
	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/msync"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/samber/lo"
	"github.com/samber/mo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"golang.org/x/sync/errgroup"
)

type ddlEventHandling string

const (
	defaultUserDocumentSize = 1024

	// The number of batches we’ll hold in memory at once.
	batchChanBufferSize = 100

	onDDLEventAllow ddlEventHandling = "allow"

	changeReaderCollectionName = "changeReader"
)

type changeReader interface {
	getWhichCluster() whichCluster
	getReadChannel() <-chan changeEventBatch
	getStartTimestamp() bson.Timestamp
	getLastSeenClusterTime() option.Option[bson.Timestamp]
	getEventsPerSecond() option.Option[float64]
	getLag() option.Option[time.Duration]
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

	startAtTs *bson.Timestamp

	lag              *msync.TypedAtomic[option.Option[time.Duration]]
	batchSizeHistory *history.History[int]

	createIteratorCb func(context.Context, *mongo.Session) (bson.Timestamp, error)
	iterateCb        func(context.Context, *retry.FuncInfo, *mongo.Session) error

	onDDLEvent ddlEventHandling
}

func newChangeReaderCommon(clusterName whichCluster) ChangeReaderCommon {
	return ChangeReaderCommon{
		readerType:           clusterName,
		changeEventBatchChan: make(chan changeEventBatch, batchChanBufferSize),
		writesOffTs:          util.NewEventual[bson.Timestamp](),
		lag:                  msync.NewTypedAtomic(option.None[time.Duration]()),
		lastChangeEventTime:  msync.NewTypedAtomic(option.None[bson.Timestamp]()),
		batchSizeHistory:     history.New[int](time.Minute),
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

// getLag returns the observed change stream lag (i.e., the delta between
// cluster time and the most-recently-seen change event).
func (rc *ChangeReaderCommon) getLag() option.Option[time.Duration] {
	return rc.lag.Load()
}

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

// start starts the change reader
func (rc *ChangeReaderCommon) start(
	ctx context.Context,
	eg *errgroup.Group,
) error {
	// This channel holds the first change stream creation's result, whether
	// success or failure. Rather than using a Result we could make separate
	// Timestamp and error channels, but the single channel is cleaner since
	// there's no chance of "nonsense" like both channels returning a payload.
	initialCreateResultChan := make(chan mo.Result[bson.Timestamp])

	eg.Go(
		func() error {
			// Closing changeEventBatchChan at the end of change stream goroutine
			// notifies the verifier's change event handler to exit.
			defer func() {
				rc.logger.Debug().
					Str("reader", string(rc.readerType)).
					Msg("Finished.")

				close(rc.changeEventBatchChan)
			}()

			retryer := retry.New().WithErrorCodes(util.CursorKilledErrCode)

			parentThreadWaiting := true

			err := retryer.WithCallback(
				func(ctx context.Context, ri *retry.FuncInfo) error {
					sess, err := rc.watcherClient.StartSession()
					if err != nil {
						return errors.Wrap(err, "failed to start session")
					}

					startTs, err := rc.createIteratorCb(ctx, sess)
					if err != nil {
						logEvent := rc.logger.Debug().
							Err(err).
							Str("reader", string(rc.readerType))

						if parentThreadWaiting {
							logEvent.Msg("First change stream open failed.")

							initialCreateResultChan <- mo.Err[bson.Timestamp](err)
							return nil
						}

						logEvent.Msg("Retried change stream open failed.")

						return err
					}

					logEvent := rc.logger.Debug().
						Str("reader", string(rc.readerType)).
						Any("startTimestamp", startTs)

					if parentThreadWaiting {
						logEvent.Msg("First change stream open succeeded.")

						initialCreateResultChan <- mo.Ok(startTs)
						close(initialCreateResultChan)
						parentThreadWaiting = false
					} else {
						logEvent.Msg("Retried change stream open succeeded.")
					}

					return rc.iterateCb(ctx, ri, sess)
				},
				"reading %s’s changes", rc.readerType,
			).Run(ctx, rc.logger)

			return err
		},
	)

	result := <-initialCreateResultChan

	startTs, err := result.Get()
	if err != nil {
		return errors.Wrapf(err, "creating change stream")
	}

	rc.startAtTs = &startTs

	rc.running = true

	return nil
}

func (rc *ChangeReaderCommon) persistResumeToken(ctx context.Context, token bson.Raw) error {
	ts, err := rc.resumeTokenTSExtractor(token)
	if err != nil {
		return errors.Wrapf(err, "parsing resume token %#q", token)
	}

	if ts.IsZero() {
		panic("empty ts in resume token is invalid!")
	}

	coll := rc.metaDB.Collection(changeReaderCollectionName)
	_, err = coll.ReplaceOne(
		ctx,
		bson.D{{"_id", resumeTokenDocID(rc.getWhichCluster())}},
		token,
		options.Replace().SetUpsert(true),
	)

	if err != nil {
		return errors.Wrapf(err, "persisting %s resume token (%v)", rc.readerType, token)
	}

	logEvent := rc.logger.Debug()

	logEvent = addTimestampToLogEvent(ts, logEvent)

	logEvent.Msgf("Persisted %s’s resume token.", rc.readerType)

	return nil
}

func resumeTokenDocID(clusterType whichCluster) string {
	switch clusterType {
	case src:
		return "srcResumeToken"
	case dst:
		return "dstResumeToken"
	default:
		panic("unknown readerType: " + clusterType)
	}
}

func (rc *ChangeReaderCommon) getMetadataCollection() *mongo.Collection {
	return rc.metaDB.Collection(changeReaderCollectionName)
}

func (rc *ChangeReaderCommon) loadResumeToken(ctx context.Context) (option.Option[bson.Raw], error) {
	coll := rc.getMetadataCollection()

	token, err := coll.FindOne(
		ctx,
		bson.D{{"_id", resumeTokenDocID(rc.getWhichCluster())}},
	).Raw()

	if errors.Is(err, mongo.ErrNoDocuments) {
		return option.None[bson.Raw](), nil
	}

	return option.Some(token), err
}

func (rc *ChangeReaderCommon) updateLag(sess *mongo.Session, token bson.Raw) {
	tokenTs, err := rc.resumeTokenTSExtractor(token)
	if err == nil {
		lagSecs := int64(sess.OperationTime().T) - int64(tokenTs.T)
		rc.lag.Store(option.Some(time.Second * time.Duration(lagSecs)))
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
