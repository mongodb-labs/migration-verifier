package api

import (
	"context"
	"slices"

	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/option"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// MigrationVerifierAPI represents the interaction webserver with mongosync
type MigrationVerifierAPI interface {
	Check(ctx context.Context, filter bson.D)
	WritesOff(ctx context.Context) error
	WritesOn(ctx context.Context)
	GetProgress(ctx context.Context) (Progress, error)
	SendDocumentMismatches(context.Context, uint32, chan<- DocMismatchInfo) error
	SendNamespaceMismatches(
		context.Context,
		[]IndexSpecTolerance,
		chan<- NSMismatchInfo,
	) error
}

type IndexSpecTolerance string

type AnyMismatchInfo interface {
	DocMismatchInfo | NSMismatchInfo
}

const (
	IndexSpecIgnoreTTL    IndexSpecTolerance = "expireAfterSeconds"
	IndexSpecIgnoreUnique IndexSpecTolerance = "unique"
)

var indexMismatchTolerances = [...]IndexSpecTolerance{
	IndexSpecIgnoreTTL,
	IndexSpecIgnoreUnique,
}

func IndexMismatchTolerances() []IndexSpecTolerance {
	return slices.Clone(indexMismatchTolerances[:])
}

// VerificationStatus holds the Verification Status
type VerificationStatus struct {
	TotalTasks            int `bson:"totalTasks"`
	AddedTasks            int `bson:"addedTasks"`
	ProcessingTasks       int `bson:"processingTasks"`
	FailedTasks           int `bson:"failedTasks"`
	CompletedTasks        int `bson:"completedTasks"`
	MetadataMismatchTasks int `bson:"metadataMismatchTasks"`
}

type ProgressGenerationStats struct {
	DocsCompared types.DocumentCount `bson:"docsCompared"`
	TotalDocs    types.DocumentCount `bson:"totalDocs"`

	SrcBytesCompared types.ByteCount `bson:"srcBytesCompared"`
	TotalSrcBytes    types.ByteCount `bson:"totalSrcBytes,omitempty"`

	TotalNamespaces types.NamespaceCount `bson:"totalNamespaces"`
}

type ProgressChangeStats struct {
	EventsPerSecond  option.Option[float64] `bson:"eventsPerSecond"`
	LagSecs          option.Option[int]     `bson:"lagSecs"`
	BufferSaturation float64                `bson:"bufferSaturation"`
}

type ProgressMismatch struct {
	DurationSeconds float64 `bson:"durationSeconds"`
	Type            string  `bson:"type"`
	Namespace       string  `bson:"namespace"`
	ID              any     `bson:"_id"`
	Detail          string  `bson:"detail"`
}

// ChangeEventCounts tallies cumulative change events seen by a change reader,
// across all generations, since the verifier first started.
type ChangeEventCounts struct {
	Insert  int
	Update  int
	Replace int
	Delete  int
}

func (cec ChangeEventCounts) Total() int {
	return cec.Insert + cec.Update + cec.Replace + cec.Delete
}

// Progress represents the structure of the JSON response from the Progress end point.
type Progress struct {
	Phase string `bson:"phase"`

	Generation      int                     `bson:"generation"`
	GenerationStats ProgressGenerationStats `bson:"generationStats"`

	// Gen0Stats holds the final doc/byte counts from generation 0 (the initial
	// check). It is absent during generation 0 itself and is populated on the
	// first /progress call after generation 0 completes.
	Gen0Stats option.Option[ProgressGenerationStats] `bson:"gen0Stats,omitempty"`

	RecentRecheckSecs []float64 `bson:"recentRecheckSecs,omitempty"`

	Error  error               `bson:"error"`
	Status *VerificationStatus `bson:"verificationStatus"`

	SrcLastRecheckedTS option.Option[bson.Timestamp] `bson:"srcLastRecheckedTS"`
	DstLastRecheckedTS option.Option[bson.Timestamp] `bson:"dstLastRecheckedTS"`

	SrcChangeEventCounts ChangeEventCounts `bson:"srcChangeEventCounts"`
	DstChangeEventCounts ChangeEventCounts `bson:"dstChangeEventCounts"`

	SrcChangeStats ProgressChangeStats `bson:"srcChangeStats"`
	DstChangeStats ProgressChangeStats `bson:"dstChangeStats"`

	DocsComparedPerSecond     float64 `bson:"docsComparedPerSecond"`
	SrcBytesComparedPerSecond float64 `bson:"srcBytesComparedPerSecond"`

	LongestDocMismatch option.Option[DocMismatchInfo] `bson:"longestDocMismatch,omitempty"`
}

type (
	MismatchType     string
	NSMismatchAspect string
)

const (
	MismatchExtra   MismatchType = "extraOnDst"
	MismatchMissing MismatchType = "missingOnDst"
	MismatchContent MismatchType = "content"

	NSMismatchAspectExist    NSMismatchAspect = "exist"
	NSMismatchAspectType     NSMismatchAspect = "type"
	NSMismatchAspectIndex    NSMismatchAspect = "index"
	NSMismatchAspectSpec     NSMismatchAspect = "spec"
	NSMismatchAspectShardKey NSMismatchAspect = "shard key"
	NSMismatchAspectReadOnly NSMismatchAspect = "readOnly"
)

type DocMismatchInfo struct {
	Type         MismatchType
	Namespace    string
	ID           bson.RawValue         `bson:"_id"`
	Field        option.Option[string] `bson:",omitempty"`
	Detail       option.Option[string] `bson:",omitempty"`
	DurationSecs float64               `bson:"durationSecs"`
}

type NSMismatchInfo struct {
	Type      MismatchType
	Namespace string
	Aspect    NSMismatchAspect
	Component option.Option[string] `bson:",omitempty"`
	Detail    option.Option[string] `bson:",omitempty"`
}

// Response is the schema for Operational API response
type Response struct {
	Success          bool    `json:"success"`
	Error            *string `json:"error,omitempty"`
	ErrorDescription *string `json:"errorDescription,omitempty"`
}
