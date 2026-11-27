package verifier

// This file holds logic to gather statistics on the verification.

import (
	"context"

	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/mongodb-labs/migration-tools/mongotools/expr"
	"github.com/mongodb-labs/migration-tools/mongotools/expr/accum"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// NamespaceStats represents progress statistics for a single namespace.
type NamespaceStats struct {
	// Namespace is "$dbname.$collname", e.g., "db1.coll1"
	Namespace string `bson:"_id"`

	// Statistics for the source’s documents
	DocsCompared, TotalDocs types.DocumentCount

	// Byte sizes for the compared source documents
	BytesCompared, TotalBytes types.ByteCount

	// Number of tasks in the relevant tasks.Status.
	TasksAdded, TasksProcessing, TasksDone uint32
}

func perNsStatsPipeline(generation int) mongo.Pipeline {
	isVerifyDocs := expr.Eq{"$type", tasks.VerifyDocuments}
	isVerifyColl := expr.Eq{"$type", tasks.VerifyCollection}

	isFinishedTask := expr.Not{expr.In("$status", tasks.Added, tasks.Processing)}

	isFinishedDocsTask := expr.And{
		isVerifyDocs,
		isFinishedTask,
	}

	oneIf := func(cond any) expr.Cond {
		return expr.Cond{If: cond, Then: 1, Else: 0}
	}

	return mongo.Pipeline{
		{{"$match", bson.D{
			{"type", bson.D{{"$in", mslices.Of(
				tasks.VerifyDocuments,
				tasks.VerifyCollection,
				tasks.PartitionIDRange,
			)}}},
			{"generation", generation},
		}}},
		{{"$addFields", bson.D{
			{"taskAdded", oneIf(expr.Eq{"$status", tasks.Added})},
			{"taskProcessing", oneIf(expr.Eq{"$status", tasks.Processing})},
			{"taskDone", oneIf(isFinishedDocsTask)},

			// If (and only if) a verify-docs task is finished, its
			// documents are done. In generation 0 we don’t actually
			// need to check task status (since verify-docs tasks only
			// have document & byte counts added once they’re done), but
			// for simplicity we might as well treat all generations the
			// same here.
			{"docsCompared", expr.Cond{
				If:   isFinishedDocsTask,
				Then: "$found_source_documents_count",
				Else: 0,
			}},
			{"bytesCompared", expr.Cond{
				If:   isFinishedDocsTask,
				Then: "$source_bytes_count",
				Else: 0,
			}},

			// In generation 0 we can get the total docs from the
			// verify-collection tasks.
			//
			// In generation 1+ we total the docs from the # of documents
			// to recheck.
			{"totalDocs", expr.Cond{
				If: expr.Eq{"$generation", 0},
				Then: expr.Cond{
					If:   isVerifyColl,
					Then: "$documents_count",
					Else: 0,
				},
				Else: expr.Cond{
					If:   isVerifyDocs,
					Then: "$documents_count",
					Else: 0,
				},
			}},

			// As with totalDocs, in generation 0 we can get totalBytes from
			// the verify-collection tasks.
			//
			// Ideally we could also approach later generations as we do with
			// totalDocs; however, the source_bytes_count figures for those
			// tasks aren’t meaningful because change events don’t always let
			// us know document size. So, after generation 0 we have to report
			// totalBytes as 0.
			{"totalBytes", expr.Cond{
				If:   isVerifyColl,
				Then: "$source_bytes_count",
				Else: 0,
			}},
		}}},
		{{"$group", bson.D{
			{"_id", "$query_filter.namespace"},
			{"docsCompared", accum.Sum{"$docsCompared"}},
			{"totalDocs", accum.Sum{"$totalDocs"}},
			{"bytesCompared", accum.Sum{"$bytesCompared"}},
			{"totalBytes", accum.Sum{"$totalBytes"}},
			{"tasksAdded", accum.Sum{"$taskAdded"}},
			{"tasksProcessing", accum.Sum{"$taskProcessing"}},
			{"tasksDone", accum.Sum{"$taskDone"}},
		}}},
		{{"$sort", bson.D{{"_id", 1}}}},
	}
}

// GetPersistedNamespaceStatistics queries the verifier’s metadata for statistics
// on progress for each namespace. The returned array is sorted by
// Namespace and contains one entry for each namespace.
func (verifier *Verifier) GetPersistedNamespaceStatistics(
	ctx context.Context,
	generation int,
) ([]NamespaceStats, error) {
	taskCollection := verifier.verificationTaskCollection()

	cursor, err := taskCollection.Aggregate(ctx, perNsStatsPipeline(generation))
	if err != nil {
		return nil, err
	}

	results := []NamespaceStats{}
	err = cursor.All(ctx, &results)
	if err != nil {
		return nil, err
	}

	return results, nil
}
