package verifier

// ------------------------------------------------------------------
// NOTE: The oplog reader sometimes triggers “extra” rechecks:
// - The first events may reflect writes that were already finalized
//   when verification started
// - If a multi-statement transaction aborts, the oplog reader will
//   still broadcast change events for the relevant documents.
//
// This is OK, of course, because extra rechecks pose no durability concerns;
// at worse, they’re just inefficient--and, we assume, trivially so.
// ------------------------------------------------------------------

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/10gen/migration-verifier/agg"
	"github.com/10gen/migration-verifier/agg/helpers"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/internal/verifier/namespaces"
	"github.com/10gen/migration-verifier/internal/verifier/oplog"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/10gen/migration-verifier/mmongo"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/mongodb-labs/migration-tools/bsontools"
	"github.com/mongodb-labs/migration-tools/option"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/x/bsonx/bsoncore"
)

const (
	ChangeReaderOptOplog = "tailOplog"
)

// Since oplog mode only supports replica sets, these are the only DDL
// events we expect to support.
var ddlCmdNameToOpType = map[string]string{
	"create":  "create",
	"collMod": "modify",

	// With an empty collection we see this:
	"createIndexes": "createIndexes",

	// With a full collection we see this:
	"commitIndexBuild": "createIndexes",

	"dropIndexes": "dropIndexes",
}

var indexCmdsToDiscard = []string{
	"startIndexBuild",
	"abortIndexBuild",
}

// OplogReader reads change events via oplog tailing instead of a change stream.
// This significantly lightens server load and allows verification of heavier
// workloads than change streams allow. It only works with replica sets.
type OplogReader struct {
	ChangeReaderCommon

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
	common := lo.TernaryF(
		cluster == dst,
		newDstChangeReaderCommon,
		func() ChangeReaderCommon {
			return newSrcChangeReaderCommon(v.ddlHandling)
		},
	)

	common.namespaces = namespaces
	common.watcherClient = client
	common.clusterInfo = clusterInfo

	common.logger = v.logger
	common.metaDB = v.metaClient.Database(v.metaDBName)

	common.resumeTokenTSExtractor = oplog.GetRawResumeTokenTimestamp

	o := &OplogReader{ChangeReaderCommon: common}

	o.createIteratorCb = o.createCursor
	o.iterateCb = o.iterateCursor

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

	var startMsg string

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
		startMsg = "Resuming oplog reader."
	} else {
		// NB: We don’t support two-phase transaction commit yet, so we just
		// start tailing after the latest optime. See REP-7181 for what will be
		// needed to enable that support.
		_, latestOpTime, err := oplog.GetTailingStartTimes(ctx, o.watcherClient)
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

		startTS = latestOpTime.TS
		startMsg = "Starting oplog reader."

		err = o.persistResumeToken(ctx, oplog.ResumeToken{startTS}.MarshalToBSON())
		if err != nil {
			return bson.Timestamp{}, errors.Wrap(err, "persisting resume token")
		}
	}

	o.logger.Info().
		Any("reader", o.getWhichCluster()).
		Any("startReadTs", startTS).
		Any("currentOplogTs", allowDDLBeforeTS).
		Msg(startMsg)

	sctx := mongo.NewSessionContext(ctx, sess)

	findOpts := options.Find().
		SetCursorType(options.TailableAwait)

	if util.ClusterHasBSONSize([2]int(o.clusterInfo.VersionArray)) {
		findOpts.SetProjection(o.getExprProjection())
	} else {
		findOpts.SetOplogReplay(true) //nolint:staticcheck
	}

	oplogFilter := bson.D{{"$and", []any{
		bson.D{{"ts", bson.D{{"$gt", startTS}}}},

		bson.D{{"$expr", agg.Or{
			// plain ops: one write per op
			append(
				agg.And{agg.In("$op", mslices.Of("d", "i", "u"))},
				o.getNSFilter("$$ROOT")...,
			),

			// op=n is for no-ops, so we stay up-to-date.
			agg.Eq{"$op", "n"},

			// op=c is for applyOps, and also to detect forbidden DDL.
			agg.And{
				agg.Eq{"$op", "c"},
				agg.Not{helpers.StringHasPrefix{
					FieldRef: "$ns",
					Prefix:   "config.",
				}},
			},
		}}},
	}}}

	cursor, err := o.watcherClient.
		Database("local").
		Collection(
			"oplog.rs",
			options.Collection().SetReadConcern(readconcern.Majority()),
		).
		Find(
			sctx,
			oplogFilter,
			findOpts,
		)
	if err != nil {
		return bson.Timestamp{}, errors.Wrapf(err, "opening cursor to tail %s’s oplog", o.readerType)
	}

	o.cursor = cursor
	o.allowDDLBeforeTS = allowDDLBeforeTS

	return startTS, nil
}

func (o *OplogReader) getExprProjection() bson.D {
	return bson.D{
		{"ts", 1},
		{"op", agg.Cond{
			If: agg.And{
				agg.Eq{"$op", "u"},
				helpers.Exists{"$o._id"},
			},
			Then: "r",
			Else: "$op",
		}},
		{"ns", 1},

		{"docLen", getOplogDocLenExpr("$$ROOT")},

		{"docID", getOplogDocIDExpr("$$ROOT")},

		{"cmdName", agg.Cond{
			If: agg.Eq{"$op", "c"},
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
				agg.Eq{"$op", "c"},
				agg.Eq{"missing", agg.Type{"$o.applyOps"}},
			},
			Then: "$o",
			Else: "$$REMOVE",
		}},

		{"ops", agg.Cond{
			If: agg.And{
				agg.Eq{"$op", "c"},
				agg.Eq{agg.Type{"$o.applyOps"}, "array"},
			},
			Then: agg.Map{
				Input: agg.Filter{
					Input: "$o.applyOps",
					As:    "opEntry",
					Cond:  o.getNSFilter("$$opEntry"),
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
	}
}

func (o *OplogReader) ddlAllowanceDocID() string {
	return string(o.readerType) + "-ddlAllowanceTS"
}

func (o *OplogReader) iterateCursor(
	ctx context.Context,
	sn retry.SuccessNotifier,
	sess *mongo.Session,
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
		case <-o.writesOffTS.Ready():
			o.logger.Debug().
				Stringer("reader", o).
				Any("timestamp", o.writesOffTS.Get()).
				Msg("Received writes-off timestamp.")

			break CursorLoop
		default:
			err = o.readAndHandleOneBatch(sctx, cursor, allowDDLBeforeTS)
			if err != nil {
				return err
			}

			sn.NoteSuccess("handled batch of ops")
		}
	}

	writesOffTS := o.writesOffTS.Get()

	for {
		if !o.lastChangeEventTime.Load().OrZero().Before(writesOffTS) {
			o.logger.Debug().
				Stringer("reader", o).
				Any("lastChangeEventTS", o.lastChangeEventTime.Load()).
				Any("writesOffTS", writesOffTS).
				Msg("Reached writes-off timestamp.")

			break
		}

		err := o.readAndHandleOneBatch(sctx, cursor, allowDDLBeforeTS)
		if err != nil {
			return err
		}
	}

	o.running = false

	infoLog := o.logger.Info()
	if ts, has := o.lastChangeEventTime.Load().Get(); has {
		infoLog = infoLog.Any("lastEventTime", ts)
		o.startAtTS = lo.ToPtr(ts)
	} else {
		infoLog = infoLog.Str("lastEventTime", "none")
	}

	infoLog.
		Stringer("reader", o).
		Msg("Oplog reader is done.")

	return nil
}

var oplogOpToOperationType = map[string]string{
	"i": "insert",
	"r": "replace", // NB: This doesn’t happen in the oplog; we project it.
	"u": "update",
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

	sess := mongo.SessionFromContext(sctx)
	o.updateLastSeenClusterTime(sess)

	if len(o.curDocs) == 0 {
		// If there were no oplog events--not even a no-op--then there’s
		// nothing for us to do.
		return nil
	}

	if o.logger.Trace().Enabled() {
		o.logger.Trace().
			Str("changeReader", string(o.getWhichCluster())).
			Strs("events", mslices.Map1(
				o.curDocs,
				bson.Raw.String,
			)).
			Int("batchEvents", len(o.curDocs)).
			Int("batchBytes", len(o.scratch)).
			Msg("Received a batch of oplog events.")
	}

	var latestTS bson.Timestamp

	events := make([]ParsedEvent, 0, len(o.curDocs))

	if util.ClusterHasBSONSize([2]int(o.clusterInfo.VersionArray)) {
		events, latestTS, err = o.parseExprProjectedOps(events, allowDDLBeforeTS)
	} else {
		events, latestTS, err = o.parseRawOps(events, allowDDLBeforeTS)
	}

	if err != nil {
		return err
	}

	// Persist the last processed timestamp; the resume query uses $gt startTS,
	// so on restart we resume strictly after the events we've already counted.
	resumeToken := oplog.ResumeToken{latestTS}.MarshalToBSON()

	// NB: In the oplog reader we can’t do this on an empty response because
	// our “resume token” requires an oplog entry to construct.
	o.updateTimestamps(sess, resumeToken)

	select {
	case <-sctx.Done():
		return err
	case o.eventBatchChan <- eventBatch{
		events:      events,
		resumeToken: resumeToken,
	}:
	}

	o.lastChangeEventTime.Store(option.Some(latestTS))

	return nil
}

func (o *OplogReader) parseRawOps(events []ParsedEvent, allowDDLBeforeTS bson.Timestamp) ([]ParsedEvent, bson.Timestamp, error) {
	var latestTS bson.Timestamp

	nsPrefixesToExclude := o.getExcludedNSPrefixes()

	parseOneDocumentOp := func(opName string, ts bson.Timestamp, rawDoc bson.Raw) error {
		nsStr, err := mbson.Lookup[string](rawDoc, "ns")
		if err != nil {
			return err
		}

		// Things we always ignore:
		for _, prefix := range nsPrefixesToExclude {
			if strings.HasPrefix(nsStr, prefix) {
				return nil
			}
		}

		// Honor namespace filtering:
		if len(o.namespaces) > 0 && !slices.Contains(o.namespaces, nsStr) {
			return nil
		}

		var docID bson.RawValue
		var docLength option.Option[types.ByteCount]
		var docField string

		switch opName {
		case "i":
			docField = "o"
		case "d":
			docID, err = rawDoc.LookupErr("o", "_id")
			if err != nil {
				return errors.Wrap(err, "extracting o._id from delete")
			}
		case "u":
			_, err := rawDoc.LookupErr("o", "_id")
			if err == nil {
				// replace, so we have the full doc
				docField = "o"
			} else if errors.Is(err, bsoncore.ErrElementNotFound) {
				docID, err = rawDoc.LookupErr("o2", "_id")
				if err != nil {
					return errors.Wrap(err, "extracting o2._id from update")
				}
			} else {
				return errors.Wrap(err, "extracting o._id from update")
			}
		default:
			panic(fmt.Sprintf("op=%#q unexpected (%v)", opName, rawDoc))
		}

		if docField != "" {
			if opName == "u" {
				opName = "r"
			}

			doc, err := mbson.Lookup[bson.Raw](rawDoc, docField)
			if err != nil {
				return errors.Wrap(err, "extracting doc from op")
			}

			docLength = option.Some(types.ByteCount(len(doc)))
			docID, err = doc.LookupErr("_id")
			if err != nil {
				return errors.Wrap(err, "extracting doc ID from op")
			}
		} else {
			if docID.IsZero() {
				panic("zero doc ID!")
			}
		}

		docID.Value = slices.Clone(docID.Value)

		events = append(
			events,
			ParsedEvent{
				OpType:      oplogOpToOperationType[opName],
				Ns:          NewNamespace(mmongo.SplitNamespace(nsStr)),
				DocID:       option.Some(docID),
				FullDocLen:  docLength,
				ClusterTime: lo.ToPtr(ts),
			},
		)

		return nil
	}

	for _, rawDoc := range o.curDocs {
		opName, err := mbson.Lookup[string](rawDoc, "op")
		if err != nil {
			return nil, bson.Timestamp{}, err
		}

		err = mbson.LookupTo(rawDoc, &latestTS, "ts")
		if err != nil {
			return nil, bson.Timestamp{}, err
		}

		switch opName {
		case "n":
			// Ignore.
		case "c":
			oDoc, err := mbson.Lookup[bson.Raw](rawDoc, "o")
			if err != nil {
				return nil, bson.Timestamp{}, err
			}

			el, err := oDoc.IndexErr(0)
			if err != nil {
				return nil, bson.Timestamp{}, errors.Wrap(err, "getting first el of o doc")
			}

			cmdName, err := bsoncore.Element(el).KeyBytesErr()
			if err != nil {
				return nil, bson.Timestamp{}, errors.Wrap(err, "getting first field name of o doc")
			}

			var isSupportedDDL bool
			isSupportedDDL, events, err = o.tryAppendDDLEvent(events, string(cmdName), oDoc, rawDoc, latestTS)
			if err != nil {
				return nil, bson.Timestamp{}, err
			}
			if isSupportedDDL {
				continue
			}

			if string(cmdName) != "applyOps" {
				err := o.getUnknownEventErr(string(cmdName), rawDoc, latestTS, allowDDLBeforeTS)
				if err != nil {
					return nil, bson.Timestamp{}, err
				}

				continue
			}

			// A transaction, or vectored writes outside a txn.
			var opsArray bson.Raw
			err = mbson.UnmarshalElementValue(el, &opsArray)
			if err != nil {
				return nil, bson.Timestamp{}, errors.Wrap(err, "parsing applyOps")
			}

			arrayVals, err := opsArray.Values()
			if err != nil {
				return nil, bson.Timestamp{}, errors.Wrap(err, "getting applyOps values")
			}

			// Might as well ...
			events = slices.Grow(events, len(arrayVals))

			for i, opRV := range arrayVals {
				opRaw, err := mbson.CastRawValue[bson.Raw](opRV)
				if err != nil {
					return nil, bson.Timestamp{}, errors.Wrapf(err, "extracting applyOps[%d]", i)
				}

				opName, err := mbson.Lookup[string](opRaw, "op")
				if err != nil {
					return nil, bson.Timestamp{}, errors.Wrapf(err, "extracting applyOps[%d].op", i)
				}

				err = parseOneDocumentOp(opName, latestTS, opRaw)
				if err != nil {
					return nil, bson.Timestamp{}, errors.Wrapf(err, "processing applyOps[%d]", i)
				}
			}
		default:
			err := parseOneDocumentOp(opName, latestTS, rawDoc)
			if err != nil {
				return nil, bson.Timestamp{}, err
			}
		}
	}

	return events, latestTS, nil
}

func (o *OplogReader) parseExprProjectedOps(events []ParsedEvent, allowDDLBeforeTS bson.Timestamp) ([]ParsedEvent, bson.Timestamp, error) {
	var latestTS bson.Timestamp

	for _, rawDoc := range o.curDocs {
		var op oplog.Op

		if err := (&op).UnmarshalFromBSON(rawDoc); err != nil {
			return nil, bson.Timestamp{}, errors.Wrapf(err, "reading oplog entry")
		}

		latestTS = op.TS

		switch op.Op {
		case "n":
			// Ignore.
		case "c":
			cmdName, has := op.CmdName.Get()
			if !has {
				return nil, bson.Timestamp{}, fmt.Errorf("no cmdname in op=c: %+v", op)
			}

			var isSupportedDDL bool
			var err error
			isSupportedDDL, events, err = o.tryAppendDDLEvent(events, cmdName, op.Object, rawDoc, op.TS)
			if err != nil {
				return nil, bson.Timestamp{}, err
			}
			if isSupportedDDL {
				continue
			}

			if cmdName != "applyOps" {
				err := o.getUnknownEventErr(cmdName, rawDoc, op.TS, allowDDLBeforeTS)
				if err != nil {
					return nil, bson.Timestamp{}, err
				}

				continue
			}

			// A transaction, or vectored writes outside a txn.
			events = append(
				events,
				lo.Map(
					op.Ops,
					func(subOp oplog.Op, _ int) ParsedEvent {
						return ParsedEvent{
							OpType:      oplogOpToOperationType[subOp.Op],
							Ns:          NewNamespace(mmongo.SplitNamespace(subOp.Ns)),
							DocID:       option.Some(subOp.DocID),
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
					Ns:          NewNamespace(mmongo.SplitNamespace(op.Ns)),
					DocID:       option.Some(op.DocID),
					FullDocLen:  option.Some(types.ByteCount(op.DocLen)),
					ClusterTime: &op.TS,
				},
			)
		}
	}

	return events, latestTS, nil
}

// tryAppendDDLEvent checks whether cmdName is a known DDL command. In warnMost
// mode it appends a ParsedEvent to events so recheck_persist can warn and count
// it. Returns (events, true, nil) when the command was DDL (caller should skip
// to the next op), or (events, false, nil) otherwise.
func (o *OplogReader) tryAppendDDLEvent(
	events []ParsedEvent,
	cmdName string,
	oDoc bson.Raw,
	rawDoc bson.Raw,
	ts bson.Timestamp,
) (bool, []ParsedEvent, error) {

	if slices.Contains(indexCmdsToDiscard, cmdName) {
		o.logger.Debug().
			Str("reader", string(o.readerType)).
			RawJSON("event", []byte(rawDoc.String())).
			Msg("Ignoring internal index event.")

		return true, events, nil
	}

	ddlOpType, isSupportedDDL := ddlCmdNameToOpType[cmdName]
	if !isSupportedDDL {
		return false, events, nil
	}
	if o.onDDLEvent != onDDLEventWarnMost {
		return false, events, nil
	}

	collName, err := bsontools.RawLookup[string](oDoc, cmdName)
	if err != nil {
		return true, events, errors.Wrap(err, "getting DDL collection name")
	}

	nsStr, err := bsontools.RawLookup[string](rawDoc, "ns")
	if err != nil {
		return true, events, errors.Wrap(err, "getting namespace")
	}

	dbName, _ := mmongo.SplitNamespace(nsStr)

	o.warnSourceDDL(rawDoc)

	return true, append(events, ParsedEvent{
		OpType:      ddlOpType,
		Ns:          NewNamespace(dbName, collName),
		DocID:       option.None[bson.RawValue](),
		ClusterTime: lo.ToPtr(ts),
	}), nil
}

func (o *OplogReader) getUnknownEventErr(
	cmdName string,
	rawDoc bson.Raw,
	ts bson.Timestamp,
	allowDDLBeforeTS bson.Timestamp,
) error {
	if o.onDDLEvent == onDDLEventAllow {
		o.logIgnoredDestDDL(rawDoc)
		return nil
	}

	if !ts.After(allowDDLBeforeTS) {
		o.logger.Info().
			Stringer("event", rawDoc).
			Msg("Ignoring unrecognized write from the past.")

		return nil
	}

	_, allowedInWarnMost := ddlCmdNameToOpType[cmdName]
	return UnknownEventError{Event: rawDoc, AllowedInWarnMost: allowedInWarnMost}
}

func (o *OplogReader) getExcludedNSPrefixes() []string {
	return append(
		slices.Clone(namespaces.ExcludedDBPrefixes),

		o.metaDB.Name()+".",
		"config.",
		"admin.",
	)
}

func (o *OplogReader) getNSFilter(docroot string) agg.And {
	filter := agg.And(lo.Map(
		o.getExcludedNSPrefixes(),
		func(prefix string, _ int) any {
			return agg.Not{helpers.StringHasPrefix{
				FieldRef: docroot + ".ns",
				Prefix:   prefix,
			}}
		},
	))

	if len(o.namespaces) > 0 {
		filter = append(
			filter,
			agg.In(docroot+".ns", o.namespaces),
		)
	}

	return filter
}

func getOplogDocLenExpr(docroot string) any {
	return agg.Cond{
		If: agg.Or{
			agg.Eq{docroot + ".op", "i"},
			agg.And{
				agg.Eq{docroot + ".op", "u"},
				helpers.Exists{docroot + ".o._id"},
			},
		},
		Then: agg.BSONSize{docroot + ".o"},
		Else: "$$REMOVE",
	}
}

func getOplogDocIDExpr(docroot string) any {
	return agg.Switch{
		Branches: []agg.SwitchCase{
			{
				Case: agg.Eq{docroot + ".op", "c"},
				Then: "$$REMOVE",
			},
			{
				Case: agg.In(docroot+".op", mslices.Of("i", "d")),
				Then: docroot + ".o._id",
			},
			{
				Case: agg.Eq{docroot + ".op", "u"},
				Then: docroot + ".o2._id",
			},
		},
	}
}

func (o *OplogReader) String() string {
	return fmt.Sprintf("%s oplog reader", o.readerType)
}
