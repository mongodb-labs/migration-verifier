package verifier

import (
	"context"
	"fmt"

	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	generationCollName  = "generation"
	generationFieldName = "generation"
)

type generationDoc struct {
	Generation      int `bson:"generation"`
	MetadataVersion int
}

func (v *Verifier) persistGenerationWhileLocked(ctx context.Context) error {
	generation, _ := v.getGenerationWhileLocked()

	db := v.verificationDatabase()

	result, err := db.Collection(generationCollName).ReplaceOne(
		ctx,
		bson.D{},
		generationDoc{
			Generation:      generation,
			MetadataVersion: verifierMetadataVersion,
		},
		options.Replace().SetUpsert(true),
	)

	if err == nil && (result.ModifiedCount+result.UpsertedCount != 1) {
		panic(fmt.Sprintf("persist of generation (%d) should affect exactly 1 doc! (%+v)", generation, result))
	}

	return err
}

func (v *Verifier) readGeneration(ctx context.Context) (option.Option[int], error) {
	db := v.verificationDatabase()

	result := db.Collection(generationCollName).FindOne(
		ctx,
		bson.D{},
	)

	parsed := generationDoc{}

	err := result.Decode(&parsed)

	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			err = nil
		} else {
			err = errors.Wrap(err, "failed to read persisted generation")
		}

		return option.None[int](), err
	}

	if parsed.MetadataVersion != verifierMetadataVersion {
		return option.None[int](), errors.Errorf(
			"persisted metadata (version: %d) predates this migration-verifier build (metadata version: %d); please discard prior verification progress by restarting with the `--clean` flag",
			parsed.MetadataVersion,
			verifierMetadataVersion,
		)
	}

	return option.Some(parsed.Generation), nil
}
