package verifier

import (
	"context"

	"github.com/10gen/migration-verifier/internal/types"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	recheckQueue   = "recheckQueue"
	maxBSONObjSize = 16 * 1024 * 1024
)

// RecheckPrimaryKey stores the implicit type of recheck to perform
// Currently, we only handle document mismatches/change stream updates,
// so DatabaseName, CollectionName, and DocumentID must always be specified.
type RecheckPrimaryKey struct {
	Generation     int         `bson:"generation"`
	DatabaseName   string      `bson:"db"`
	CollectionName string      `bson:"coll"`
	DocumentID     interface{} `bson:"docID"`
}

// RecheckDoc stores the necessary information to know which documents must be rechecked.
type RecheckDoc struct {
	PrimaryKey RecheckPrimaryKey `bson:"_id"`
	DataSize   int               `bson:"dataSize"`
}

// InsertFailedCompareRecheckDocs is for inserting RecheckDocs based on failures during Check.
func (verifier *Verifier) InsertFailedCompareRecheckDocs(
	namespace string, documentIDs []interface{}, dataSizes []int) error {
	dbName, collName := SplitNamespace(namespace)
	return verifier.insertRecheckDocs(context.Background(),
		dbName, collName, documentIDs, dataSizes)
}

// AddAndMaybeFlushChangeEventRecheckDoc adds a recheck document to the verifier's in-memory recheck buffer.
// It may flush the buffer for that namespace if it reaches
func (verifier *Verifier) AddAndMaybeFlushChangeEventRecheckDoc(ctx context.Context, changeEvent *ParsedEvent) error {
	namespace := changeEvent.Ns.String()
	verifier.changeEventRecheckBuf.buf[namespace] = append(verifier.changeEventRecheckBuf.buf[namespace], changeEvent.DocKey.ID)

	bsonID, err := bson.Marshal(changeEvent.DocKey.ID)
	if err != nil {
		return err
	}
	verifier.changeEventRecheckBuf.bufSize[namespace] += uint64(len(namespace) + len(bsonID))

	// Flush all recheck documents once a buffer reaches 5 MB.
	if verifier.changeEventRecheckBuf.bufSize[changeEvent.Ns.String()] > 5*1024*1024 {
		if err := verifier.flushChangeEventRechecksForNamespace(ctx, namespace); err != nil {
			return err
		}
		verifier.changeEventRecheckBuf.bufSize[namespace] = 0
	}
	return nil
}

func (verifier *Verifier) insertRecheckDocs(
	ctx context.Context,
	dbName, collName string, documentIDs []interface{}, dataSizes []int) error {
	verifier.mux.Lock()
	defer verifier.mux.Unlock()

	generation, _ := verifier.getGenerationWhileLocked()

	models := []mongo.WriteModel{}
	for i, documentID := range documentIDs {
		pk := RecheckPrimaryKey{
			Generation:     generation,
			DatabaseName:   dbName,
			CollectionName: collName,
			DocumentID:     documentID,
		}

		// The filter must exclude DataSize; otherwise, if a failed comparison
		// and a change event happen on the same document for the same
		// generation, the 2nd insert will fail because a) its filter won’t
		// match anything, and b) it’ll try to insert a new document with the
		// same _id as the one that the 1st insert already created.
		filterDoc := bson.D{{"_id", pk}}

		recheckDoc := RecheckDoc{
			PrimaryKey: pk,
			DataSize:   dataSizes[i],
		}

		models = append(models,
			mongo.NewReplaceOneModel().
				SetFilter(filterDoc).SetReplacement(recheckDoc).SetUpsert(true))
	}
	_, err := verifier.verificationDatabase().Collection(recheckQueue).BulkWrite(ctx, models)

	if err == nil {
		verifier.logger.Debug().Msgf("Persisted %d recheck doc(s) for generation %d", len(models), generation)
	}

	// Silence any duplicate key errors as recheck docs should have existed.
	if mongo.IsDuplicateKeyError(err) {
		err = nil
	}

	return err
}

// ClearRecheckDocs deletes the previous generation’s recheck
// documents from the verifier’s metadata.
//
// The verifier **MUST** be locked when this function is called (or panic).
func (verifier *Verifier) ClearRecheckDocs(ctx context.Context) error {
	prevGeneration := verifier.getPreviousGenerationWhileLocked()

	verifier.logger.Debug().Msgf("Deleting generation %d’s %s documents", prevGeneration, recheckQueue)

	_, err := verifier.verificationDatabase().Collection(recheckQueue).DeleteMany(
		ctx, bson.D{{"_id.generation", prevGeneration}})
	return err
}

func (verifier *Verifier) getPreviousGenerationWhileLocked() int {
	generation, _ := verifier.getGenerationWhileLocked()
	if generation < 1 {
		panic("This function is forbidden before generation 1!")
	}

	return generation - 1
}

// GenerateRecheckTasks fetches the previous generation’s recheck
// documents from the verifier’s metadata and creates current-generation
// document-verification tasks from them.
//
// The verifier **MUST** be locked when this function is called (or panic).
func (verifier *Verifier) GenerateRecheckTasks(ctx context.Context) error {
	prevGeneration := verifier.getPreviousGenerationWhileLocked()

	verifier.logger.Debug().Msgf("Creating recheck tasks from generation %d’s %s documents", prevGeneration, recheckQueue)

	// We generate one recheck task per collection, unless
	// 1) The size of the list of IDs would exceed 12MB (a very conservative way of avoiding
	//    the 16MB BSON limit)
	// 2) The size of the data would exceed our desired partition size.  This limits memory use
	//    during the recheck phase.
	prevDBName, prevCollName := "", ""
	var idAccum []interface{}
	var idLenAccum int
	var dataSizeAccum int64
	const maxIdsSize = 12 * 1024 * 1024
	cursor, err := verifier.verificationDatabase().Collection(recheckQueue).Find(
		ctx, bson.D{{"_id.generation", prevGeneration}}, options.Find().SetSort(bson.D{{"_id", 1}}))
	if err != nil {
		return err
	}
	defer cursor.Close(ctx)
	// We group these here using a sort rather than using aggregate because aggregate is
	// subject to a 16MB limit on group size.
	for cursor.Next(ctx) {
		err := cursor.Err()
		if err != nil {
			return err
		}
		var doc RecheckDoc
		err = cursor.Decode(&doc)
		if err != nil {
			return err
		}
		idRaw := cursor.Current.Lookup("_id", "docID")
		idLen := len(idRaw.Value)

		verifier.logger.Debug().Msgf("Found persisted recheck doc for %s.%s", doc.PrimaryKey.DatabaseName, doc.PrimaryKey.CollectionName)

		if doc.PrimaryKey.DatabaseName != prevDBName ||
			doc.PrimaryKey.CollectionName != prevCollName ||
			idLenAccum >= maxIdsSize ||
			dataSizeAccum >= verifier.partitionSizeInBytes {
			namespace := prevDBName + "." + prevCollName
			if len(idAccum) > 0 {
				err := verifier.InsertFailedIdsVerificationTask(idAccum, types.ByteCount(dataSizeAccum), namespace)
				if err != nil {
					return err
				}
				verifier.logger.Debug().Msgf(
					"Created ID verification task for namespace %s with %d ids, "+
						"%d id bytes and %d data bytes",
					namespace, len(idAccum), idLenAccum, dataSizeAccum)
			}
			prevDBName = doc.PrimaryKey.DatabaseName
			prevCollName = doc.PrimaryKey.CollectionName
			idLenAccum = 0
			dataSizeAccum = 0
			idAccum = []interface{}{}
		}
		idLenAccum += idLen
		dataSizeAccum += int64(doc.DataSize)
		idAccum = append(idAccum, doc.PrimaryKey.DocumentID)
	}
	if len(idAccum) > 0 {
		namespace := prevDBName + "." + prevCollName
		err := verifier.InsertFailedIdsVerificationTask(idAccum, types.ByteCount(dataSizeAccum), namespace)
		if err != nil {
			return err
		}
		verifier.logger.Debug().Msgf(
			"Created ID verification task for namespace %s with %d ids, "+
				"%d id bytes and %d data bytes",
			namespace, len(idAccum), idLenAccum, dataSizeAccum)
	}
	return nil
}

func (verifier *Verifier) flushAllBufferedChangeEventRechecks(ctx context.Context) error {
	for namespace, _ := range verifier.changeEventRecheckBuf.buf {
		if err := verifier.flushChangeEventRechecksForNamespace(ctx, namespace); err != nil {
			return err
		}
	}

	return nil
}

func (verifier *Verifier) flushChangeEventRechecksForNamespace(ctx context.Context, namespace string) error {
	ids := verifier.changeEventRecheckBuf.buf[namespace]

	if len(ids) == 0 {
		return nil
	}

	// We don't know the document sizes for documents for all change events,
	// so just be conservative and assume they are maximum size.
	//
	// Note that this prevents us from being able to report a meaningful
	// total data size for noninitial generations in the log.
	dataSizes := make([]int, len(ids))
	for i, _ := range ids {
		dataSizes[i] = maxBSONObjSize
	}

	dbName, collName := SplitNamespace(namespace)
	return verifier.insertRecheckDocs(ctx, dbName, collName, ids, dataSizes)
}
