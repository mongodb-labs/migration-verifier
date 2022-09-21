package verifier

import (
	"context"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	recheckQueue = "recheckQueue"
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
	generation, _ := verifier.getGeneration()
	dbName, collName := SplitNamespace(namespace)
	return verifier.insertRecheckDocs(context.Background(), generation,
		dbName, collName, documentIDs, dataSizes)
}

func (verifier *Verifier) InsertChangeEventRecheckDoc(ctx context.Context, changeEvent *ParsedEvent) error {
	generation, _ := verifier.getGeneration()
	documentIDs := []interface{}{changeEvent.DocKey.ID}
	// We don't know the document sizes for documents for all change events, so just be conservative
	// and assume they are maximum size.
	dataSizes := []int{16 * 1024 * 1024}
	return verifier.insertRecheckDocs(
		ctx, generation, changeEvent.Ns.DB, changeEvent.Ns.Coll, documentIDs, dataSizes)
}

func (verifier *Verifier) insertRecheckDocs(
	ctx context.Context, generation int,
	dbName, collName string, documentIDs []interface{}, dataSizes []int) error {
	models := []mongo.WriteModel{}
	for i, documentID := range documentIDs {
		pk := RecheckPrimaryKey{
			Generation:     generation,
			DatabaseName:   dbName,
			CollectionName: collName,
			DocumentID:     documentID,
		}
		recheckDoc := RecheckDoc{
			PrimaryKey: pk,
			DataSize:   dataSizes[i],
		}
		models = append(models,
			mongo.NewReplaceOneModel().
				SetFilter(recheckDoc).SetReplacement(recheckDoc).SetUpsert(true))
	}
	_, err := verifier.verificationDatabase().Collection(recheckQueue).BulkWrite(ctx, models)
	return err
}

func (verifier *Verifier) ClearRecheckDocs(ctx context.Context, generation int) error {
	_, err := verifier.verificationDatabase().Collection(recheckQueue).DeleteMany(
		ctx, bson.D{{"_id.generation", generation}})
	return err
}

func (verifier *Verifier) GenerateRecheckTasks(ctx context.Context, generation int) error {
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
		ctx, bson.D{{"_id.generation", generation}}, options.Find().SetSort(bson.D{{"_id", 1}}))
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
		if doc.PrimaryKey.DatabaseName != prevDBName ||
			doc.PrimaryKey.CollectionName != prevCollName ||
			idLenAccum >= maxIdsSize ||
			dataSizeAccum >= verifier.partitionSizeInBytes {
			namespace := prevDBName + "." + prevCollName
			if len(idAccum) > 0 {
				err := verifier.InsertFailedIdsVerificationTask(idAccum, namespace)
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
		err := verifier.InsertFailedIdsVerificationTask(idAccum, namespace)
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
