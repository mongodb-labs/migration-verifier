package verifier

import (
	"context"
	"fmt"

	"github.com/10gen/migration-verifier/agg"
	"github.com/10gen/migration-verifier/agg/helpers"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/internal/verifier/namespaces"
	"github.com/10gen/migration-verifier/internal/verifier/oplog"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/10gen/migration-verifier/mmongo"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"golang.org/x/exp/slices"
)

// OplogReader reads change events via oplog tailing instead of a change stream.
// This significantly lightens server load and allows verification of heavier
// workloads than change streams allow. It only works with replica sets.
type OplogReader struct {
	*ChangeReaderCommon

	curDocs          []bson.Raw
	scratch          []byte
	cursor           *mongo.Cursor
	allowDDLBeforeTS bson.Timestamp
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
	common.watcherClient = client
	common.clusterInfo = clusterInfo

	common.logger = v.logger
	common.metaDB = v.metaClient.Database(v.metaDBName)

	common.resumeTokenTSExtractor = oplog.GetRawResumeTokenTimestamp

	o := &OplogReader{ChangeReaderCommon: &common}

	common.createIteratorCb = o.createCursor
	common.iterateCb = o.iterateCursor

	return o
}

func (o *OplogReader) createCursor(
	ctx context.Context,
	sess *mongo.Session,
) (bson.Timestamp, error) {
	savedResumeToken, err := o.loadResumeToken(ctx)
	if err != nil {
		return bson.Timestamp{}, errors.Wrap(err, "loading persisted resume token")
	}

	var allowDDLBeforeTS bson.Timestamp

	var startTS bson.Timestamp

	if token, has := savedResumeToken.Get(); has {
		var rt oplog.ResumeToken
		if err := bson.Unmarshal(token, &rt); err != nil {
			return bson.Timestamp{}, errors.Wrap(err, "parsing persisted resume token")
		}

		ddlAllowanceResult := o.getMetadataCollection().FindOne(
			ctx,
			bson.D{
				{"_id", o.ddlAllowanceDocID()},
			},
		)

		allowanceRaw, err := ddlAllowanceResult.Raw()
		if err != nil {
			return bson.Timestamp{}, errors.Wrap(err, "fetching DDL allowance timestamp")
		}

		allowDDLBeforeTS, err = mbson.Lookup[bson.Timestamp](allowanceRaw, "ts")
		if err != nil {
			return bson.Timestamp{}, errors.Wrap(err, "parsing DDL allowance timestamp doc")
		}

		startTS = rt.TS
	} else {
		startOpTime, latestOpTime, err := oplog.GetTailingStartTimes(ctx, o.watcherClient)
		if err != nil {
			return bson.Timestamp{}, errors.Wrapf(err, "getting start optime from %s", o.readerType)
		}

		allowDDLBeforeTS = latestOpTime.TS

		_, err = o.getMetadataCollection().ReplaceOne(
			ctx,
			bson.D{
				{"_id", o.ddlAllowanceDocID()},
			},
			bson.D{
				{"ts", allowDDLBeforeTS},
			},
			options.Replace().SetUpsert(true),
		)
		if err != nil {
			return bson.Timestamp{}, errors.Wrapf(err, "persisting DDL-allowance timestamp")
		}

		startTS = startOpTime.TS

		err = o.persistResumeToken(ctx, oplog.ResumeToken{startTS}.MarshalToBSON())
		if err != nil {
			return bson.Timestamp{}, errors.Wrap(err, "persisting resume token")
		}
	}

	o.logger.Info().
		Any("startReadTs", startTS).
		Any("currentOplogTs", allowDDLBeforeTS).
		Msg("Tailing oplog.")

	sctx := mongo.NewSessionContext(ctx, sess)

	cursor, err := o.watcherClient.
		Database("local").
		Collection(
			"oplog.rs",
			options.Collection().SetReadConcern(readconcern.Majority()),
		).
		Find(
			sctx,
			bson.D{{"$and", []any{
				bson.D{{"ts", bson.D{{"$gte", startTS}}}},

				bson.D{{"$expr", agg.Or{
					// plain ops: one write per op
					append(
						agg.And{agg.In("$op", "d", "i", "u")},
						o.getDefaultNSExclusions("$$ROOT")...,
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
					{"ns", 1},

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

					{"o", agg.Cond{
						If: agg.And{
							agg.Eq("$op", "c"),
							agg.Eq("missing", agg.Type("$o.applyOps")),
						},
						Then: "$o",
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
								Cond:  o.getDefaultNSExclusions("$$opEntry"),
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
		return bson.Timestamp{}, errors.Wrapf(err, "opening cursor to tail %s’s oplog", o.readerType)
	}

	o.cursor = cursor
	o.allowDDLBeforeTS = allowDDLBeforeTS

	return startTS, nil
}

func (o *OplogReader) ddlAllowanceDocID() string {
	return string(o.readerType) + "-ddlAllowanceTS"
}

func (o *OplogReader) iterateCursor(
	ctx context.Context,
	_ *retry.FuncInfo,
	sess *mongo.Session,
	/*
		cursor *mongo.Cursor,
		allowDDLBeforeTS bson.Timestamp,
	*/
) error {
	sctx := mongo.NewSessionContext(ctx, sess)
	cursor := o.cursor
	allowDDLBeforeTS := o.allowDDLBeforeTS

CursorLoop:
	for {
		var err error

		select {
		case <-sctx.Done():
			return sctx.Err()
		case <-o.writesOffTs.Ready():
			break CursorLoop
		default:
			err = o.readAndHandleOneBatch(sctx, cursor, allowDDLBeforeTS)
			if err != nil {
				return err
			}
		}
	}

	writesOffTS := o.writesOffTs.Get()

	for {
		if o.lastChangeEventTime != nil {
			if !o.lastChangeEventTime.Before(writesOffTS) {
				fmt.Printf("----------- %s reached writes off ts %v", o, writesOffTS)
				break
			}
		}

		err := o.readAndHandleOneBatch(sctx, cursor, allowDDLBeforeTS)
		if err != nil {
			return err
		}
	}

	// TODO: deduplicate
	o.running = false

	infoLog := o.logger.Info()
	if o.lastChangeEventTime != nil {
		infoLog = infoLog.Any("lastEventTime", o.lastChangeEventTime)
		o.startAtTs = lo.ToPtr(*o.lastChangeEventTime)
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
	allowDDLBeforeTS bson.Timestamp,
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

				if !op.TS.After(allowDDLBeforeTS) {
					o.logger.Info().
						Stringer("event", rawDoc).
						Msg("Ignoring unrecognized write from the past.")

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

	sess := mongo.SessionFromContext(sctx)
	resumeToken := oplog.ResumeToken{latestTS}.MarshalToBSON()

	o.updateLag(sess, resumeToken)

	o.batchSizeHistory.Add(len(events))

	select {
	case <-sctx.Done():
		return err
	case o.changeEventBatchChan <- changeEventBatch{
		events:      events,
		resumeToken: resumeToken,
		clusterTime: *sess.OperationTime(),
	}:
	}

	o.lastChangeEventTime = &latestTS

	return nil
}

func (o *OplogReader) getDefaultNSExclusions(docroot string) agg.And {
	prefixes := append(
		slices.Clone(namespaces.MongosyncMetaDBPrefixes),
		o.metaDB.Name()+".",
		"config.",
		"admin.",
	)

	return agg.And(lo.Map(
		prefixes,
		func(prefix string, _ int) any {
			return agg.Not{helpers.StringHasPrefix{
				FieldRef: docroot + ".ns",
				Prefix:   prefix,
			}}
		},
	))
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
	return fmt.Sprintf("%s oplog reader", o.readerType)
}
