package verifier

import (
	"context"
	"fmt"
	"sync"

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
	DatabaseName   string      `bson:"db"`
	CollectionName string      `bson:"coll"`
	DocumentID     interface{} `bson:"docID"`
}

// RecheckDoc stores the necessary information to know which documents must be rechecked.
type RecheckDoc struct {
	PrimaryKey RecheckPrimaryKey `bson:"_id"`
}

func (verifier *Verifier) insertCheckDoc(recheckDoc *RecheckDoc) error {
	replaceOptions := options.Replace().SetUpsert(true)
	_, err := verifier.metaClientCollection(recheckQueue).ReplaceOne(context.Background(), recheckDoc, recheckDoc, replaceOptions)
	return err
}

// InsertChangeEventRecheckDoc is for inserting RecheckDocs based on change stream events
// into the RecheckQueue.
func (verifier *Verifier) InsertChangeEventRecheckDoc(changeEvent *ParsedEvent) error {
	pk := RecheckPrimaryKey{
		DatabaseName:   changeEvent.Ns.DB,
		CollectionName: changeEvent.Ns.Coll,
		DocumentID:     changeEvent.DocKey.ID,
	}
	recheckDoc := RecheckDoc{
		PrimaryKey: pk,
	}
	return verifier.insertCheckDoc(&recheckDoc)
}

// InsertFailedCompareRecheckDoc is for inserting RecheckDocs based on failures during Check.
func (verifier *Verifier) InsertFailedCompareRecheckDoc(dbName, collName string, documentID interface{}) error {
	pk := RecheckPrimaryKey{
		DatabaseName:   dbName,
		CollectionName: collName,
		DocumentID:     documentID,
	}
	recheckDoc := RecheckDoc{
		PrimaryKey: pk,
	}
	return verifier.insertCheckDoc(&recheckDoc)
}

// RecheckAggregate is for aggregating all failed docs for a given Namespace into
// one document.
type RecheckAggregate struct {
	ID  Namespace     `bson:"_id"`
	Ids []interface{} `bson:"_ids"`
}

func (ra *RecheckAggregate) NamespaceString() string {
	return ra.ID.DB + "." + ra.ID.Coll
}

// GetRecheckDocs returns a cursor of the RecheckAggregates we can farm out to Recheck workers.
// Each one contains all the _ids which mismatched for each Namespace.
func (verifier *Verifier) GetRecheckDocs(ctx context.Context) (*mongo.Cursor, error) {
	t := true
	opts := options.AggregateOptions{
		AllowDiskUse: &t,
	}
	coll := verifier.metaClientCollection(recheckQueue)
	return coll.Aggregate(ctx,
		[]interface{}{
			bson.M{
				"$group": bson.M{
					"_id": bson.M{
						"db":   "$_id.db",
						"coll": "$_id.coll",
					},
					"_ids": bson.M{
						"$push": "$_id.docID",
					},
				},
			},
			bson.M{
				"$sort": bson.M{"_id": 1},
			},
		},
		&opts)
}

// Recheck runs the functionality for the /recheck endpoint, which rechecks any documents that
// failed during Check or which were seen to change in the change stream.
func (verifier *Verifier) Recheck(ctx context.Context) error {
	verifier.logger.Info().Msg("Starting Recheck")
	verifier.changeStreamMux.Lock()
	csRunning := verifier.changeStreamRunning
	verifier.changeStreamMux.Unlock()
	if csRunning {
		verifier.logger.Info().Msg("Changestream still running, sinalling that writes are done and waiting for change stream to exit")
		verifier.changeStreamEnderChan <- struct{}{}
		select {
		case err := <-verifier.changeStreamErrChan:
			return err
		case <-verifier.changeStreamDoneChan:
			break
		}
	}
	// set the phase once the change stream is exhausted
	verifier.phase = Recheck
	defer func() {
		verifier.phase = Idle
	}()

	recheckChan := make(chan *RecheckAggregate)
	doneChan := make(chan struct{})
	errChan := make(chan error)
	wg := sync.WaitGroup{}

	worker := func() {
		wg.Add(1)
		defer wg.Done()
		verficationTask := VerificationTask{}
		for {
			select {
			case <-ctx.Done():
				return
			case recheck := <-recheckChan:
				verficationTask.QueryFilter = QueryFilter{Namespace: recheck.NamespaceString()}
				verficationTask.Ids = recheck.Ids
				res, err := verifier.FetchAndCompareDocuments(&verficationTask)
				if err != nil {
					verifier.logger.Error().Err(err).Msg("Error during recheck")
					errChan <- err
					return
				}
				if len(res) != 0 {
					for _, verificationResult := range res {
						verifier.logger.Info().Msg(fmt.Sprintf("Documents failed verification in recheck: %v", verificationResult))
					}
				}
			case <-doneChan:
				return
			}
		}
	}

	for i := 0; i < verifier.numWorkers; i++ {
		go worker()
	}

	cur, err := verifier.GetRecheckDocs(ctx)
	if err != nil {
		return err
	}
	for cur.Next(ctx) {
		recheck := RecheckAggregate{}
		err = cur.Decode(&recheck)
		if err != nil {
			return err
		}
		recheckChan <- &recheck
	}
	for i := 0; i < verifier.numWorkers; i++ {
		doneChan <- struct{}{}
	}

	wg.Wait()
	select {
	case err := <-errChan:
		return err
	default:
		verifier.logger.Info().Msg("Recheck finished")
		return nil
	}
}
