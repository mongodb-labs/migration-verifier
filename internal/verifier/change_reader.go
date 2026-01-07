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

type readerCurrentTimes struct {
	LastHandledTime   bson.Timestamp `json:"lastHandledTime"`
	LastOperationTime bson.Timestamp `json:"lastOperationTime"`
}

func (rp readerCurrentTimes) Lag() time.Duration {
	return time.Second * time.Duration(
		int(rp.LastOperationTime.T)-int(rp.LastHandledTime.T),
	)
}

type changeReader interface {
	getWhichCluster() whichCluster
	getReadChannel() <-chan eventBatch
	getEventRecorder() *EventRecorder
	getStartTimestamp() bson.Timestamp
	getLastSeenClusterTime() option.Option[bson.Timestamp]
	getEventsPerSecond() option.Option[float64]
	getCurrentTimes() option.Option[readerCurrentTimes]
	getBufferSaturation() float64
	noteBatchSize(int)
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

	eventRecorder *EventRecorder

	resumeTokenTSExtractor func(bson.Raw) (bson.Timestamp, error)

	running        bool
	eventBatchChan chan eventBatch
	writesOffTs    *util.Eventual[bson.Timestamp]

	lastChangeEventTime *msync.TypedAtomic[option.Option[bson.Timestamp]]

	currentTimes *msync.TypedAtomic[option.Option[readerCurrentTimes]]

	startAtTs *bson.Timestamp

	batchSizeHistory *history.History[int]

	createIteratorCb func(context.Context, *mongo.Session) (bson.Timestamp, error)
	iterateCb        func(context.Context, retry.SuccessNotifier, *mongo.Session) error

	onDDLEvent ddlEventHandling
}

func newChangeReaderCommon(clusterName whichCluster) ChangeReaderCommon {
	return ChangeReaderCommon{
		readerType:          clusterName,
		eventBatchChan:      make(chan eventBatch, batchChanBufferSize),
		eventRecorder:       NewEventRecorder(),
		writesOffTs:         util.NewEventual[bson.Timestamp](),
		currentTimes:        msync.NewTypedAtomic(option.None[readerCurrentTimes]()),
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

func (rc *ChangeReaderCommon) getEventRecorder() *EventRecorder {
	return rc.eventRecorder
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

func (rc *ChangeReaderCommon) getReadChannel() <-chan eventBatch {
	return rc.eventBatchChan
}

func (rc *ChangeReaderCommon) getLastSeenClusterTime() option.Option[bson.Timestamp] {
	return rc.lastChangeEventTime.Load()
}

// getBufferSaturation returns the reader’s internal buffer’s saturation level
// as a fraction. If saturation rises, that means we’re reading events faster
// than we can persist them.
func (rc *ChangeReaderCommon) getBufferSaturation() float64 {
	return util.DivideToF64(len(rc.eventBatchChan), cap(rc.eventBatchChan))
}

func (rc *ChangeReaderCommon) getCurrentTimes() option.Option[readerCurrentTimes] {
	return rc.currentTimes.Load()
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

func (rc *ChangeReaderCommon) noteBatchSize(size int) {
	rc.batchSizeHistory.Add(size)
}

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

				close(rc.eventBatchChan)
			}()

			retryer := retry.New().WithErrorCodes(util.CursorKilledErrCode)

			parentThreadWaiting := true

			err := retryer.WithCallback(
				func(ctx context.Context, ri *retry.FuncInfo) error {
					sess, err := rc.watcherClient.StartSession()
					if err != nil {
						return errors.Wrap(err, "failed to start session")
					}

					if rc.createIteratorCb == nil {
						panic("rc.createIteratorCb should be set")
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
	if len(token) == 0 {
		panic("internal error: resume token is empty but should never be")
	}

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

	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			err = nil
		}

		return option.None[bson.Raw](), err
	}

	return option.Some(token), nil
}

func (rc *ChangeReaderCommon) updateTimes(sess *mongo.Session, token bson.Raw) {
	tokenTs, err := rc.resumeTokenTSExtractor(token)
	if err == nil {
		opTime := sess.OperationTime()

		if opTime == nil {
			panic("session operationTime is nil … did this get called prematurely?")
		}

		rc.currentTimes.Store(option.Some(readerCurrentTimes{
			LastHandledTime:   tokenTs,
			LastOperationTime: *opTime,
		}))
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
