package verifier

import (
	"context"
	"fmt"

	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/mongodb-labs/migration-tools/option"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	generationCollName = "generation"
)

type generationDoc struct {
	Generation                 int
	MetadataVersion            int
	SourceChangeReaderOpt      string
	DestinationChangeReaderOpt string
}

type metadataMismatchErr struct {
	persistedVersion int
}

func (mme metadataMismatchErr) Error() string {
	return fmt.Sprintf("persisted metadata (version: %d) predates this migration-verifier build (metadata version: %d); please discard prior verification progress by restarting with the `--clean` flag",
		mme.persistedVersion,
		verifierMetadataVersion,
	)
}

type changeReaderOptMismatchErr struct {
	reader       whichCluster
	persistedOpt string
	currentOpt   string
}

func (crme changeReaderOptMismatchErr) Error() string {
	return fmt.Sprintf("new %s change reader opt is %#q, but %#q was used previously; either use the old option, or restart verification",
		crme.reader,
		crme.currentOpt,
		crme.persistedOpt,
	)
}

func (v *Verifier) persistGenerationWhileLocked(ctx context.Context) error {
	generation, _ := v.getGenerationWhileLocked()

	db := v.verificationDatabase()

	result, err := db.Collection(generationCollName).ReplaceOne(
		ctx,
		bson.D{},
		generationDoc{
			Generation:                 generation,
			MetadataVersion:            verifierMetadataVersion,
			SourceChangeReaderOpt:      v.srcChangeReaderMethod,
			DestinationChangeReaderOpt: v.dstChangeReaderMethod,
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

	var foundNothing bool
	parsed := generationDoc{}

	err := retry.New().WithCallback(
		func(ctx context.Context, _ *retry.FuncInfo) error {
				ctx,
				bson.D{},
			).Decode(&parsed)

			if errors.Is(err, mongo.ErrNoDocuments) {
				foundNothing = true
				return nil
			}

			return err
		},
		"read persisted generation",
	).Run(ctx, v.logger)

	if err != nil {
		return option.None[int](), err
	}

	if foundNothing {
		return option.None[int](), nil
	}

	if parsed.MetadataVersion != verifierMetadataVersion {
		return option.None[int](), metadataMismatchErr{parsed.MetadataVersion}
	}

	if parsed.SourceChangeReaderOpt != v.srcChangeReaderMethod {
		return option.None[int](), changeReaderOptMismatchErr{
			reader:       src,
			persistedOpt: parsed.SourceChangeReaderOpt,
			currentOpt:   v.srcChangeReaderMethod,
		}
	}

	if parsed.DestinationChangeReaderOpt != v.dstChangeReaderMethod {
		return option.None[int](), changeReaderOptMismatchErr{
			reader:       dst,
			persistedOpt: parsed.DestinationChangeReaderOpt,
			currentOpt:   v.dstChangeReaderMethod,
		}
	}

	return option.Some(parsed.Generation), nil
}
