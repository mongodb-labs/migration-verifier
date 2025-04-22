package verifier

// This file holds logic to gather statistics on the verification.

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"text/template"

	"github.com/mongodb-labs/migration-verifier/internal/types"
	"go.mongodb.org/mongo-driver/bson"
)

// NamespaceStats represents progress statistics for a single namespace.
type NamespaceStats struct {

	// Namespace is "$dbname.$collname", e.g., "db1.coll1"
	Namespace string `bson:"_id"`

	// Statistics for the source’s documents
	DocsCompared, TotalDocs types.DocumentCount

	// Byte sizes for the compared source documents
	BytesCompared, TotalBytes types.ByteCount

	// Number of partitions in the relevant verificationTaskStatus.
	PartitionsAdded, PartitionsProcessing, PartitionsDone uint32
}

// We could store this pipeline using the Go driver’s bson.D, bson.A, etc.,
// but that would be awfully ugly for a pipeline as complex as this.
// For ease of maintenance, then, we store this pipeline as JSON.
const perNsStatsPipelineTemplate = `[
	{
		"$match": {
			"type": { "$ne": "primary" },
			"generation": {{.Generation}}
		}
	},
	{
		"$addFields": {
			"partitionAdded": {
				"$cond": [
					{ "$and": [
						{ "$eq": [ "$type", "{{.VerifyDocsType}}" ] },
						{ "$eq": [ "$status", "{{.AddedStatus}}" ] }
					] },
					1,
					0
				]
			},
			"partitionProcessing": {
				"$cond": [
					{ "$and": [
						{ "$eq": [ "$type", "{{.VerifyDocsType}}" ] },
						{ "$eq": [ "$status", "{{.ProcessingStatus}}" ] }
					] },
					1,
					0
				]
			},
			"partitionDone": {
				"$cond": [
					{ "$and": [
						{ "$eq": [ "$type", "{{.VerifyDocsType}}" ] },
						{ "$in": [ "$status", [
							"{{.CompletedStatus}}",
							"{{.FailedStatus}}"
						] ] }
					] },
					1,
					0
				]
			},

			{{/*
				If (and only if) a verify-docs task is finished, its
				documents are done. In generation 0 we don’t actually
				need to check task status (since verify-docs tasks only
				have document & byte counts added once they’re done), but
				for simplicity we might as well treat all generations the
				same here.
			*/}}
			"docsCompared": {
				"$cond": [
					{ "$and": [
						{ "$eq": [ "$type", "{{.VerifyDocsType}}" ] },
						{ "$in": [ "$status", [
							"{{.CompletedStatus}}",
							"{{.FailedStatus}}"
						] ] }
					] },
					"$source_documents_count",
					0
				]
			},
			"bytesCompared": {
				"$cond": [
					{ "$and": [
						{ "$eq": [ "$type", "{{.VerifyDocsType}}" ] },
						{ "$in": [ "$status", [
							"{{.CompletedStatus}}",
							"{{.FailedStatus}}"
						] ] }
					] },
					"$source_bytes_count",
					0
				]
			},

			{{/*
				In generation 0 we can get the total docs from the
				verify-collection tasks.

				In later generations we don’t have verify-collection tasks,
				so we add up the individual recheck batch tasks.
			*/}}
			"totalDocs": {
				"$cond": [
					{ "$or": [
						{ "$eq": [ "$type", "{{.VerifyCollType}}" ] },
						{ "$and": [
							{ "$eq": [ "$type", "{{.VerifyDocsType}}" ] },
							{ "$ne": [ "$generation", 0 ] }
						] }
					] },
					"$source_documents_count",
					0
				]
			},

			{{/*
				As with totalDocs, in generation 0 we can get totalBytes from
				the verify-collection tasks.

				Ideally we could also approach later generations as we do with
				totalDocs; however, the source_bytes_count figures for those
				tasks aren’t meangingful because change stream events don’t allow
				us to determine document size. So, after generation 0 we have to
				report totalBytes as 0.
			*/}}
			"totalBytes": {
				"$cond": [
					{ "$eq": [ "$type", "{{.VerifyCollType}}" ] },
					"$source_bytes_count",
					0
				]
			}
		}
	},
	{
		"$group": {
			"_id": "$query_filter.namespace",
			"docsCompared": {"$sum": "$docsCompared"},
			"totalDocs": {"$sum": "$totalDocs"},
			"bytesCompared": {"$sum": "$bytesCompared"},
			"totalBytes": {"$sum": "$totalBytes"},
			"partitionsAdded": {"$sum": "$partitionAdded"},
			"partitionsProcessing": {"$sum": "$partitionProcessing"},
			"partitionsDone": {"$sum": "$partitionDone"}
		}
	},
	{ "$sort": { "_id": 1 } }
]`

var templateOnce sync.Once
var jsonTemplate *template.Template

// GetNamespaceStatistics queries the verifier’s metadata for statistics
// on progress for each namespace. The returned array is sorted by
// Namespace and contains one entry for each namespace.
func (verifier *Verifier) GetNamespaceStatistics(ctx context.Context) ([]NamespaceStats, error) {
	generation, _ := verifier.getGeneration()

	templateOnce.Do(func() {
		tmpl, err := template.New("").Parse(perNsStatsPipelineTemplate)
		if err != nil {
			panic(fmt.Sprintf("Bad template in source: %+v", err))
		}

		jsonTemplate = tmpl
	})

	jsonBytes := bytes.Buffer{}
	err := jsonTemplate.Execute(
		&jsonBytes,
		map[string]any{
			"Generation": generation,

			"VerifyDocsType": verificationTaskVerifyDocuments,
			"VerifyCollType": verificationTaskVerifyCollection,

			"AddedStatus":      verificationTaskAdded,
			"ProcessingStatus": verificationTaskProcessing,
			"CompletedStatus":  verificationTaskCompleted,
			"FailedStatus":     verificationTaskFailed,
		},
	)
	if err != nil {
		panic(fmt.Sprintf("Bad template run in source: %+v", err))
	}

	pipeline := []bson.M{}
	err = bson.UnmarshalExtJSON(jsonBytes.Bytes(), true, &pipeline)
	if err != nil {
		panic(fmt.Sprintf("Bad extended JSON in source: %+v", err))
	}

	taskCollection := verifier.verificationTaskCollection()

	cursor, err := taskCollection.Aggregate(ctx, pipeline)
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
