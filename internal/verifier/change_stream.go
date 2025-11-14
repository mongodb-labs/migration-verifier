package verifier

import (
	"context"
	"fmt"
	"time"

	"github.com/10gen/migration-verifier/internal/keystring"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/10gen/migration-verifier/option"
	mapset "github.com/deckarep/golang-set/v2"
	clone "github.com/huandu/go-clone/generic"
	"github.com/pkg/errors"
	"github.com/samber/mo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"golang.org/x/exp/slices"
	"golang.org/x/sync/errgroup"
)

var supportedEventOpTypes = mapset.NewSet(
	"insert",
	"update",
	"replace",
	"delete",
)

const (
	minChangeStreamPersistInterval = time.Second * 10
	maxChangeStreamAwaitTime       = time.Second
)

type UnknownEventError struct {
	Event bson.Raw
}

func (uee UnknownEventError) Error() string {
	return fmt.Sprintf("received event with unknown optype: %+v", uee.Event)
}

type ChangeStreamReader struct {
	ChangeReaderCommon
}

var _ changeReader = &ChangeStreamReader{}

// GetChangeStreamFilter returns an aggregation pipeline that filters
// namespaces as per configuration.
//
// Note that this omits verifier.globalFilter because we still need to
// recheck any out-filter documents that may have changed in order to
// account for filter traversals (i.e., updates that change whether a
// document matches the filter).
//
// NB: Ideally we could make the change stream give $bsonSize(fullDocument)
// and omit fullDocument, but $bsonSize was new in MongoDB 4.4, and we still
// want to verify migrations from 4.2. fullDocument is unlikely to be a
// bottleneck anyway.
func (csr *ChangeStreamReader) GetChangeStreamFilter() (pipeline mongo.Pipeline) {
	if len(csr.namespaces) == 0 {
		pipeline = mongo.Pipeline{
			{{"$match", util.ExcludePrefixesQuery(
				"ns.db",
				append(
					slices.Clone(MongosyncMetaDBPrefixes),
					csr.metaDB.Name(),
				),
			)}},
		}
	} else {
		filter := []bson.D{}
		for _, ns := range csr.namespaces {
			db, coll := SplitNamespace(ns)
			filter = append(filter, bson.D{
				{"ns", bson.D{
					{"db", db},
					{"coll", coll},
				}},
			})
		}
		pipeline = mongo.Pipeline{
			{{"$match", bson.D{{"$or", filter}}}},
		}
	}

	pipeline = append(
		pipeline,
		bson.D{
			{"$addFields", bson.D{
				{"_docID", "$documentKey._id"},

				{"updateDescription", "$$REMOVE"},
				{"wallTime", "$$REMOVE"},
				{"documentKey", "$$REMOVE"},
			}},
		},
	)

	if util.ClusterHasBSONSize([2]int(csr.clusterInfo.VersionArray)) {
		pipeline = append(
			pipeline,
			bson.D{
				{"$addFields", bson.D{
					{"_fullDocLen", bson.D{{"$bsonSize", "$fullDocument"}}},
					{"fullDocument", "$$REMOVE"},
				}},
			},
		)
	}

	return pipeline
}

// This function reads a single `getMore` response into a slice.
//
// Note that this doesn’t care about the writesOff timestamp. Thus,
// if writesOff has happened and a `getMore` response’s events straddle
// the writesOff timestamp (i.e., some events precede it & others follow it),
// the verifier will enqueue rechecks from those post-writesOff events. This
// is unideal but shouldn’t impede correctness since post-writesOff events
// shouldn’t really happen anyway by definition.
func (csr *ChangeStreamReader) readAndHandleOneChangeEventBatch(
	ctx context.Context,
	ri *retry.FuncInfo,
	cs *mongo.ChangeStream,
	sess *mongo.Session,
) error {
	eventsRead := 0
	var changeEvents []ParsedEvent

	latestEvent := option.None[ParsedEvent]()

	var batchTotalBytes int
	for hasEventInBatch := true; hasEventInBatch; hasEventInBatch = cs.RemainingBatchLength() > 0 {
		gotEvent := cs.TryNext(ctx)

		if cs.Err() != nil {
			return errors.Wrap(cs.Err(), "change stream iteration failed")
		}

		if !gotEvent {
			break
		}

		if changeEvents == nil {
			batchSize := cs.RemainingBatchLength() + 1

			ri.NoteSuccess("received a batch of %d change event(s)", batchSize)

			changeEvents = make([]ParsedEvent, batchSize)
		}

		batchTotalBytes += len(cs.Current)

		if err := (&changeEvents[eventsRead]).UnmarshalFromBSON(cs.Current); err != nil {
			return errors.Wrapf(err, "failed to decode change event to %T", changeEvents[eventsRead])
		}

		// This only logs in tests.
		csr.logger.Trace().
			Stringer("changeStream", csr).
			Any("event", changeEvents[eventsRead]).
			Int("eventsPreviouslyReadInBatch", eventsRead).
			Int("batchEvents", len(changeEvents)).
			Int("batchBytes", batchTotalBytes).
			Msg("Received a change event.")

		opType := changeEvents[eventsRead].OpType
		if !supportedEventOpTypes.Contains(opType) {

			// We expect certain DDL events on the destination as part of
			// a migration. For example, mongosync enables indexes’ uniqueness
			// constraints and sets capped collection sizes, and sometimes
			// indexes are created after initial sync.

			if csr.onDDLEvent == onDDLEventAllow {
				csr.logIgnoredDDL(cs.Current)

				// Discard this event, then keep reading.
				changeEvents = changeEvents[:len(changeEvents)-1]

				continue
			} else {
				return UnknownEventError{Event: clone.Clone(cs.Current)}
			}
		}

		// This shouldn’t happen, but just in case:
		if changeEvents[eventsRead].Ns == nil {
			return errors.Errorf("Change event lacks a namespace: %+v", changeEvents[eventsRead])
		}

		if changeEvents[eventsRead].ClusterTime != nil &&
			(csr.lastChangeEventTime == nil ||
				csr.lastChangeEventTime.Before(*changeEvents[eventsRead].ClusterTime)) {

			csr.lastChangeEventTime = changeEvents[eventsRead].ClusterTime
			latestEvent = option.Some(changeEvents[eventsRead])
		}

		eventsRead++
	}

	csr.updateLag(sess, cs.ResumeToken())

	if eventsRead == 0 {
		ri.NoteSuccess("received an empty change stream response")

		return nil
	}

	csr.batchSizeHistory.Add(eventsRead)

	if event, has := latestEvent.Get(); has {
		csr.logger.Trace().
			Stringer("changeStreamReader", csr).
			Any("event", event).
			Msg("Updated lastChangeEventTime.")
	}

	ri.NoteSuccess("parsed %d-event batch", len(changeEvents))

	select {
	case <-ctx.Done():
		return util.WrapCtxErrWithCause(ctx)
	case csr.changeEventBatchChan <- changeEventBatch{
		events: changeEvents,

		resumeToken: cs.ResumeToken(),

		// NB: We know by now that OperationTime is non-nil.
		clusterTime: *sess.OperationTime(),
	}:
	}

	ri.NoteSuccess("sent %d-event batch to handler", len(changeEvents))

	return nil
}

func (csr *ChangeStreamReader) iterateChangeStream(
	ctx context.Context,
	ri *retry.FuncInfo,
	cs *mongo.ChangeStream,
	sess *mongo.Session,
) error {
	for {
		var err error
		var gotwritesOffTimestamp bool

		select {

		// If the context is canceled, return immmediately.
		case <-ctx.Done():
			err := util.WrapCtxErrWithCause(ctx)

			csr.logger.Debug().
				Err(err).
				Stringer("changeStreamReader", csr).
				Msg("Stopping iteration.")

			return err

		// If the ChangeStreamEnderChan has a message, the user has indicated that
		// source writes are ended and the migration tool is finished / committed.
		// This means we should exit rather than continue reading the change stream
		// since there should be no more events.
		case <-csr.writesOffTs.Ready():
			writesOffTs := csr.writesOffTs.Get()

			csr.logger.Debug().
				Any("writesOffTimestamp", writesOffTs).
				Msgf("%s received writesOff timestamp. Finalizing change stream.", csr)

			gotwritesOffTimestamp = true

			// Read change events until the stream reaches the writesOffTs.
			// (i.e., the `getMore` call returns empty)
			for {
				var curTs bson.Timestamp
				curTs, err = csr.resumeTokenTSExtractor(cs.ResumeToken())
				if err != nil {
					return errors.Wrap(err, "failed to extract timestamp from change stream's resume token")
				}

				// writesOffTs never refers to a real event,
				// so we can stop once curTs >= writesOffTs.
				if !curTs.Before(writesOffTs) {
					csr.logger.Debug().
						Any("currentTimestamp", curTs).
						Any("writesOffTimestamp", writesOffTs).
						Msgf("%s has reached the writesOff timestamp. Shutting down.", csr)

					break
				}

				err = csr.readAndHandleOneChangeEventBatch(ctx, ri, cs, sess)

				if err != nil {
					return err
				}
			}

		default:
			err = csr.readAndHandleOneChangeEventBatch(ctx, ri, cs, sess)

			if err != nil {
				return err
			}
		}

		if gotwritesOffTimestamp {
			csr.running = false
			if csr.lastChangeEventTime != nil {
				csr.startAtTs = csr.lastChangeEventTime
			}

			break
		}
	}

	infoLog := csr.logger.Info()
	if csr.lastChangeEventTime == nil {
		infoLog = infoLog.Str("lastEventTime", "none")
	} else {
		infoLog = infoLog.Any("lastEventTime", *csr.lastChangeEventTime)
	}

	infoLog.
		Stringer("reader", csr).
		Msg("Change stream reader is done.")

	return nil
}

func (csr *ChangeStreamReader) createChangeStream(
	ctx context.Context,
) (*mongo.ChangeStream, *mongo.Session, bson.Timestamp, error) {
	pipeline := csr.GetChangeStreamFilter()
	opts := options.ChangeStream().
		SetMaxAwaitTime(maxChangeStreamAwaitTime)

	if csr.clusterInfo.VersionArray[0] >= 6 {
		opts = opts.SetCustomPipeline(
			bson.M{
				"showSystemEvents":   true,
				"showExpandedEvents": true,
			},
		)
	}

	savedResumeToken, err := csr.loadResumeToken(ctx)
	if err != nil {
		return nil, nil, bson.Timestamp{}, errors.Wrap(err, "failed to load persisted change stream resume token")
	}

	csStartLogEvent := csr.logger.Info()

	if token, hasToken := savedResumeToken.Get(); hasToken {
		logEvent := csStartLogEvent.
			Stringer(csr.resumeTokenDocID(), token)

		ts, err := csr.resumeTokenTSExtractor(token)
		if err == nil {
			logEvent = addTimestampToLogEvent(ts, logEvent)
		} else {
			csr.logger.Warn().
				Err(err).
				Msg("Failed to extract timestamp from persisted resume token.")
		}

		logEvent.Msg("Starting change stream from persisted resume token.")

		//opts = opts.SetStartAfter(token)
		opts = opts.SetResumeAfter(token)
	} else {
		csStartLogEvent.Msgf("Starting change stream from current %s cluster time.", csr.readerType)
	}

	sess, err := csr.watcherClient.StartSession()
	if err != nil {
		return nil, nil, bson.Timestamp{}, errors.Wrap(err, "failed to start session")
	}
	sctx := mongo.NewSessionContext(ctx, sess)
	changeStream, err := csr.watcherClient.Watch(sctx, pipeline, opts)
	if err != nil {
		return nil, nil, bson.Timestamp{}, errors.Wrap(err, "failed to open change stream")
	}

	err = csr.persistResumeToken(ctx, changeStream.ResumeToken())
	if err != nil {
		return nil, nil, bson.Timestamp{}, err
	}

	startTs, err := csr.resumeTokenTSExtractor(changeStream.ResumeToken())
	if err != nil {
		return nil, nil, bson.Timestamp{}, errors.Wrap(err, "failed to extract timestamp from change stream's resume token")
	}

	// With sharded clusters the resume token might lead the cluster time
	// by 1 increment. In that case we need the actual cluster time;
	// otherwise we will get errors.
	clusterTime, err := util.GetClusterTimeFromSession(sess)
	if err != nil {
		return nil, nil, bson.Timestamp{}, errors.Wrap(err, "failed to read cluster time from session")
	}

	csr.logger.Debug().
		Any("resumeTokenTimestamp", startTs).
		Any("clusterTime", clusterTime).
		Stringer("changeStreamReader", csr).
		Msg("Using earlier time as start timestamp.")

	if startTs.After(clusterTime) {
		startTs = clusterTime
	}

	return changeStream, sess, startTs, nil
}

// StartChangeStream starts the change stream.
func (csr *ChangeStreamReader) start(
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
				csr.logger.Debug().
					Stringer("changeStreamReader", csr).
					Msg("Closing change event batch channel.")

				close(csr.changeEventBatchChan)
			}()

			retryer := retry.New().WithErrorCodes(util.CursorKilledErrCode)

			parentThreadWaiting := true

			err := retryer.WithCallback(
				func(ctx context.Context, ri *retry.FuncInfo) error {
					changeStream, sess, startTs, err := csr.createChangeStream(ctx)
					if err != nil {
						logEvent := csr.logger.Debug().
							Err(err).
							Stringer("changeStreamReader", csr)

						if parentThreadWaiting {
							logEvent.Msg("First change stream open failed.")

							initialCreateResultChan <- mo.Err[bson.Timestamp](err)
							return nil
						}

						logEvent.Msg("Retried change stream open failed.")

						return err
					}

					defer changeStream.Close(ctx)

					logEvent := csr.logger.Debug().
						Stringer("changeStreamReader", csr).
						Any("startTimestamp", startTs)

					if parentThreadWaiting {
						logEvent.Msg("First change stream open succeeded.")

						initialCreateResultChan <- mo.Ok(startTs)
						close(initialCreateResultChan)
						parentThreadWaiting = false
					} else {
						logEvent.Msg("Retried change stream open succeeded.")
					}

					return csr.iterateChangeStream(ctx, ri, changeStream, sess)
				},
				"running %s", csr,
			).Run(ctx, csr.logger)

			return err
		},
	)

	result := <-initialCreateResultChan

	startTs, err := result.Get()
	if err != nil {
		return errors.Wrapf(err, "creating change stream")
	}

	csr.startAtTs = &startTs

	csr.running = true

	return nil
}

func (csr *ChangeStreamReader) String() string {
	return fmt.Sprintf("%s change stream reader", csr.readerType)
}

func extractTSFromChangeStreamResumeToken(resumeToken bson.Raw) (bson.Timestamp, error) {
	// Change stream token is always a V1 keystring in the _data field
	tokenDataRV, err := resumeToken.LookupErr("_data")

	if err != nil {
		return bson.Timestamp{}, errors.Wrapf(err, "extracting %#q from resume token (%v)", "_data", resumeToken)
	}

	tokenData, err := mbson.CastRawValue[string](tokenDataRV)
	if err != nil {
		return bson.Timestamp{}, errors.Wrapf(err, "parsing resume token (%v)", resumeToken)
	}

	resumeTokenBson, err := keystring.KeystringToBson(keystring.V1, tokenData)
	if err != nil {
		return bson.Timestamp{}, err
	}
	// First element is the cluster time we want
	resumeTokenTime, ok := resumeTokenBson[0].Value.(bson.Timestamp)
	if !ok {
		return bson.Timestamp{}, errors.Errorf("resume token data's (%+v) first element is of type %T, not a timestamp", resumeTokenBson, resumeTokenBson[0].Value)
	}

	return resumeTokenTime, nil
}
