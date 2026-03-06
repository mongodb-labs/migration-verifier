package api

import (
	"context"

	"github.com/10gen/migration-verifier/option"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// MigrationVerifierAPI represents the interaction webserver with mongosync
type MigrationVerifierAPI interface {
	Check(ctx context.Context, filter bson.D)
	WritesOff(ctx context.Context) error
	WritesOn(ctx context.Context)
	GetProgress(ctx context.Context) (Progress, error)
	SendDocumentMismatches(context.Context, uint32, chan<- MismatchInfo) error
}

// VerificationStatus holds the Verification Status
type VerificationStatus struct {
	TotalTasks            int `json:"totalTasks"`
	AddedTasks            int `json:"addedTasks"`
	ProcessingTasks       int `json:"processingTasks"`
	FailedTasks           int `json:"failedTasks"`
	CompletedTasks        int `json:"completedTasks"`
	MetadataMismatchTasks int `json:"metadataMismatchTasks"`
}

// Progress represents the structure of the JSON response from the Progress end point.
type Progress struct {
	Phase      string              `json:"phase"`
	Generation int                 `json:"generation"`
	Error      error               `json:"error"`
	Status     *VerificationStatus `json:"verificationStatus"`
}

type MismatchType string

const (
	MismatchExtra   MismatchType = "extraOnDst"
	MismatchMissing MismatchType = "missingOnDst"
	MismatchContent MismatchType = "content"
)

type MismatchInfo struct {
	Type         MismatchType
	Namespace    string
	ID           bson.RawValue         `bson:"_id"`
	Field        option.Option[string] `bson:",omitempty"`
	Detail       option.Option[string] `bson:",omitempty"`
	DurationSecs float64               `bson:"durationSecs"`
}

// Response is the schema for Operational API response
type Response struct {
	Success          bool    `json:"success"`
	Error            *string `json:"error,omitempty"`
	ErrorDescription *string `json:"errorDescription,omitempty"`
}
