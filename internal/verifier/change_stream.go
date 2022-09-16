package verifier

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ParsedEvent contains the fields of an event that we have parsed from 'bson.Raw'.
type ParsedEvent struct {
	ID     interface{} `bson:"_id"`
	OpType string      `bson:"operationType"`
	Ns     *Namespace  `bson:"ns,omitempty"`
	DocKey DocKey      `bson:"documentKey,omitempty"`
}

func (pe *ParsedEvent) String() string {
	return fmt.Sprintf("{ OpType: %s, namespace: %s, docID: %v}", pe.OpType, pe.Ns, pe.DocKey.ID)
}

// DocKey is a deserialized form for the ChangeEvent documentKey field. We currently only care about
// the _id.
type DocKey struct {
	ID interface{} `bson:"_id"`
}

// HandleChangeStreamEvent performs the necessary work for change stream events that occur during
// operation.
func (verifier *Verifier) HandleChangeStreamEvent(ctx context.Context, changeEvent *ParsedEvent) error {
	switch changeEvent.OpType {
	case "delete":
		fallthrough
	case "insert":
		fallthrough
	case "replace":
		fallthrough
	case "update":
		return verifier.InsertChangeEventIdVerificationTask(ctx, changeEvent)
	default:
		return fmt.Errorf(`Not supporting: "` + changeEvent.OpType + `" events`)
	}
}

func (verifier *Verifier) GetChangeStreamFilter() []bson.D {
	if len(verifier.srcNamespaces) == 0 {
		return []bson.D{{bson.E{"$match", bson.D{{"ns.db", bson.D{{"$ne", verifier.metaDBName}}}}}}}
	}
	filter := bson.A{}
	for _, ns := range verifier.srcNamespaces {
		db, coll := SplitNamespace(ns)
		filter = append(filter, bson.D{{"ns", bson.D{{"db", db}, {"coll", coll}}}})
	}
	stage := bson.D{{"$match", bson.D{{"$or", filter}}}}
	return []bson.D{stage}
}

// StartChangeStream starts the change stream.
func (verifier *Verifier) StartChangeStream(ctx context.Context, startTime *primitive.Timestamp) error {
	streamReader := func(cs *mongo.ChangeStream) {
		var changeEvent ParsedEvent
		for {
			select {
			// if the context is cancelled return immmediately
			case <-ctx.Done():
				return
			// if the changeStreamEnderChan has a message, we have moved to the Recheck phase, obtain
			// the remaining changes, but when TryNext returns false, we will exit, since there should
			// be no message until the user has guaranteed writes to the source have ended.
			case <-verifier.changeStreamEnderChan:
				for cs.TryNext(ctx) {
					if err := cs.Decode(&changeEvent); err != nil {
						verifier.logger.Fatal().Err(err).Msg("Failed to decode change event")
					}
					err := verifier.HandleChangeStreamEvent(ctx, &changeEvent)
					if err != nil {
						verifier.changeStreamErrChan <- err
						verifier.logger.Fatal().Err(err).Msg("Error handling change event")
					}
				}
				verifier.mux.Lock()
				verifier.changeStreamRunning = false
				verifier.mux.Unlock()
				// since we have started Recheck, we must signal that we have
				// finished the change stream changes so that Recheck can continue.
				verifier.changeStreamDoneChan <- struct{}{}
				// since the changeStream is exhausted, we now return
				verifier.logger.Debug().Msg("Change stream is done")
				return
			// the default case is that we are still in the Check phase, in the check phase we still
			// use TryNext, but we do not exit if TryNext returns false.
			default:
				if next := cs.TryNext(ctx); !next {
					continue
				}
				if err := cs.Decode(&changeEvent); err != nil {
					verifier.logger.Fatal().Err(err).Msg("")
				}
				err := verifier.HandleChangeStreamEvent(ctx, &changeEvent)
				if err != nil {
					verifier.changeStreamErrChan <- err
					return
				}
			}
		}
	}
	pipeline := verifier.GetChangeStreamFilter()
	opts := options.ChangeStream().SetMaxAwaitTime(1 * time.Second).SetStartAtOperationTime(startTime)
	srcChangeStream, err := verifier.srcClient.Watch(context.Background(), pipeline, opts)
	if err != nil {
		return err
	}
	verifier.mux.Lock()
	verifier.changeStreamRunning = true
	verifier.mux.Unlock()
	go streamReader(srcChangeStream)
	return nil
}
