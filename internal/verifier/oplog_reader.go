package verifier

import (
	"context"
	"fmt"

	"github.com/10gen/migration-verifier/agg"
	"github.com/10gen/migration-verifier/agg/helpers"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/internal/verifier/oplog"
	"github.com/10gen/migration-verifier/mmongo"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
)

// OplogReader reads change events via oplog tailing instead of a change stream.
// This significantly lightens server load and allows verification of heavier
// workloads than change streams allow. It only works with replica sets.
type OplogReader struct {
	curDocs []bson.Raw
	scratch []byte
	ChangeReaderCommon
}

var _ changeReader = &OplogReader{}

func (v *Verifier) newOplogReader(
	namespaces []string,
	cluster whichCluster,
	client *mongo.Client,
	clusterInfo util.ClusterInfo,
) *OplogReader {
	common := newChangeReaderCommon(cluster)
	common.namespaces = namespaces
	common.clusterName = cluster
	common.client = client
	common.clusterInfo = clusterInfo

	common.logger = v.logger
	common.metaDB = v.metaClient.Database(v.metaDBName)

	common.resumeTokenTSExtractor = oplog.GetRawResumeTokenTimestamp

	return &OplogReader{ChangeReaderCommon: common}
}

func (o *OplogReader) start(ctx context.Context) error {
	// TODO: retryer

	savedResumeToken, err := o.loadResumeToken(ctx)
	if err != nil {
		return errors.Wrap(err, "loading persisted resume token")
	}

	if token, has := savedResumeToken.Get(); has {
		var rt oplog.ResumeToken
		if err := bson.Unmarshal(token, &rt); err != nil {
			return errors.Wrap(err, "parsing persisted resume token")
		}

		o.startAtTS = option.Some(rt.TS)
	} else {
		startOpTime, err := oplog.GetTailingStartTime(ctx, o.client)
		if err != nil {
			return errors.Wrapf(err, "getting start optime from %s", o.clusterName)
		}

		o.startAtTS = option.Some(startOpTime.TS)
	}

	sess, err := o.client.StartSession()
	if err != nil {
		return errors.Wrap(err, "creating session")
	}

	sctx := mongo.NewSessionContext(ctx, sess)

	cursor, err := o.client.
		Database("local").
		Collection(
			"oplog.rs",
			options.Collection().SetReadConcern(readconcern.Majority()),
		).
		Find(
			sctx,
			bson.D{{"$and", []any{
				bson.D{{"ts", bson.D{{"$gte", o.startAtTS.MustGet()}}}},

				bson.D{{"$expr", agg.Or{
					// plain ops: one write per op
					append(
						agg.And{agg.In("$op", "d", "i", "u")},
						getOplogDefaultNSExclusions("$$ROOT")...,
					),

					// op=c is for applyOps, and also to detect forbidden DDL.
					// op=n is for no-ops, so we stay up-to-date.
					agg.In("$op", "c", "n"),
				}}},
			}}},

			options.Find().
				SetCursorType(options.TailableAwait).
				SetProjection(bson.D{
					{"ts", 1},

					{"op", 1},

					{"ns", agg.Cond{
						If:   agg.Eq("$op", "c"),
						Then: "$$REMOVE",
						Else: "$ns",
					}},

					// TODO: Adjust for 4.2.
					{"docLen", getOplogDocLenExpr("$$ROOT")},

					{"docID", getOplogDocIDExpr("$$ROOT")},

					{"cmdName", agg.Cond{
						If: agg.Eq("$op", "c"),
						Then: agg.ArrayElemAt{
							Array: agg.Map{
								Input: bson.D{
									{"$objectToArray", "$o"},
								},
								As: "field",
								In: "$$field.k",
							},
							Index: 0,
						},
						Else: "$$REMOVE",
					}},

					{"ops", agg.Cond{
						If: agg.And{
							agg.Eq("$op", "c"),
							agg.Eq(agg.Type("$o.applyOps"), "array"),
						},
						Then: agg.Map{
							Input: agg.Filter{
								Input: "$o.applyOps",
								As:    "opEntry",
								Cond:  getOplogDefaultNSExclusions("$$opEntry"),
							},
							As: "opEntry",
							In: bson.D{
								{"op", "$$opEntry.op"},
								{"ns", "$$opEntry.ns"},
								{"docID", getOplogDocIDExpr("$$opEntry")},
								{"docLen", getOplogDocLenExpr("$$opEntry")},
							},
						},
						Else: "$$REMOVE",
					}},
				}),
		)

	if err != nil {
		return errors.Wrapf(err, "opening cursor to tail oplog")
	}

	go func() {
		if err := o.iterate(sctx, cursor); err != nil {
			o.readerError.Set(err)
		}
	}()

	return nil
}

func (o *OplogReader) iterate(
	sctx context.Context,
	cursor *mongo.Cursor,
) error {
CursorLoop:
	for {
		var err error

		select {
		case <-sctx.Done():
			return sctx.Err()
		case <-o.persistorError.Ready():
			return o.wrapPersistorErrorForReader()

		case <-o.writesOffTS.Ready():
			break CursorLoop
		default:
			err = o.readAndHandleOneBatch(sctx, cursor)
			if err != nil {
				return err
			}
		}
	}

	writesOffTS := o.writesOffTS.Get()

	for {
		if lastTime, has := o.lastChangeTime.Get(); has {
			if !lastTime.Before(writesOffTS) {
				fmt.Printf("----------- %s reached writes off ts %v", o, writesOffTS)
				break
			}
		}

		err := o.readAndHandleOneBatch(sctx, cursor)
		if err != nil {
			return err
		}
	}

	// TODO: deduplicate
	o.running = false

	// since we have started Recheck, we must signal that we have
	// finished the change stream changes so that Recheck can continue.
	close(o.doneChan)

	infoLog := o.logger.Info()
	if lastTime, has := o.lastChangeTime.Get(); has {
		infoLog = infoLog.Any("lastEventTime", lastTime)
		o.startAtTS = o.lastChangeTime.Clone()
	} else {
		infoLog = infoLog.Str("lastEventTime", "none")
	}

	infoLog.
		Stringer("reader", o).
		Msg("Change stream reader is done.")

	return nil
}

var oplogOpToOperationType = map[string]string{
	"i": "insert",
	"u": "update", // don’t need to distinguish from replace
	"d": "delete",
}

func (o *OplogReader) readAndHandleOneBatch(
	sctx context.Context,
	cursor *mongo.Cursor,
) error {
	var err error

	o.curDocs = o.curDocs[:0]
	o.scratch = o.scratch[:0]

	o.curDocs, o.scratch, err = mmongo.GetBatch(sctx, cursor, o.curDocs, o.scratch)
	if err != nil {
		return errors.Wrap(err, "reading cursor")
	}

	events := make([]ParsedEvent, 0, len(o.curDocs))

	var latestTS bson.Timestamp

	for _, rawDoc := range o.curDocs {
		var op oplog.Op

		if err := (&op).UnmarshalFromBSON(rawDoc); err != nil {
			return errors.Wrapf(err, "reading oplog entry")
		}

		latestTS = op.TS

		switch op.Op {
		case "n":
		case "c":
			if op.CmdName != "applyOps" {
				if o.onDDLEvent == onDDLEventAllow {
					o.logIgnoredDDL(rawDoc)
					continue
				}

				return UnknownEventError{rawDoc}
			}

			events = append(
				events,
				lo.Map(
					op.Ops,
					func(subOp oplog.Op, _ int) ParsedEvent {
						return ParsedEvent{
							OpType:      oplogOpToOperationType[subOp.Op],
							Ns:          NewNamespace(SplitNamespace(subOp.Ns)),
							DocID:       subOp.DocID,
							FullDocLen:  option.Some(types.ByteCount(subOp.DocLen)),
							ClusterTime: &op.TS,
						}
					},
				)...,
			)
		default:
			events = append(
				events,
				ParsedEvent{
					OpType:      oplogOpToOperationType[op.Op],
					Ns:          NewNamespace(SplitNamespace(op.Ns)),
					DocID:       op.DocID,
					FullDocLen:  option.Some(types.ByteCount(op.DocLen)),
					ClusterTime: &op.TS,
				},
			)
		}
	}

	if len(events) == 0 {
		return nil
	}

	sess := mongo.SessionFromContext(sctx)
	resumeToken := oplog.ResumeToken{latestTS}.MarshalToBSON()

	o.updateLag(sess, resumeToken)

	o.batchSizeHistory.Add(len(events))

	select {
	case <-sctx.Done():
		return err
	case <-o.persistorError.Ready():
		return o.wrapPersistorErrorForReader()
	case o.eventsChan <- changeEventBatch{
		events:      events,
		resumeToken: resumeToken,
		clusterTime: *sess.OperationTime(),
	}:
	}

	o.lastChangeTime = option.Some(latestTS)

	return nil
}

func getOplogDefaultNSExclusions(docroot string) agg.And {
	return agg.And{
		// TODO: This logic is for all-namespace listening.
		// If ns filtering is in play we need something smarter.
		agg.Not{helpers.StringHasPrefix{
			FieldRef: docroot + ".ns",
			Prefix:   "config.",
		}},
		agg.Not{helpers.StringHasPrefix{
			FieldRef: docroot + ".ns",
			Prefix:   "admin.",
		}},
	}
}

func getOplogDocLenExpr(docroot string) any {
	return agg.Switch{
		Branches: []agg.SwitchCase{
			{
				Case: agg.Or{
					agg.Eq(docroot+".op", "i"),
					agg.And{
						agg.Eq(docroot+".op", "u"),
						agg.Not{agg.Eq("missing", docroot+".o._id")},
					},
				},
				Then: agg.BSONSize(docroot + ".o"),
			},
		},
		Default: "$$REMOVE",
	}
}

func getOplogDocIDExpr(docroot string) any {
	return agg.Switch{
		Branches: []agg.SwitchCase{
			{
				Case: agg.Eq(docroot+".op", "c"),
				Then: "$$REMOVE",
			},
			{
				Case: agg.In(docroot+".op", "i", "d"),
				Then: docroot + ".o._id",
			},
			{
				Case: agg.In(docroot+".op", "u"),
				Then: docroot + ".o2._id",
			},
		},
	}
}

func (o *OplogReader) String() string {
	return fmt.Sprintf("%s oplog reader", o.ChangeReaderCommon.clusterName)
}
