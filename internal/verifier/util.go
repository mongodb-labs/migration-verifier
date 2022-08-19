package verifier

import (
	"strings"

	"go.mongodb.org/mongo-driver/bson"
)

// Specify states
const (
	Unprocessed = "unprocessed"
	Upserted    = "upserted"
	Deleted     = "deleted"
	NotFound    = "not found"
)

// Specify error codes
const (
	ErrorGetClusterStatsSummary = 522601
	ErrorTrackedDirtyBytes
	ErrorReplicationDelayed

	ErrorInsertTask
	ErrorUpdateParentTask
	ErrorUpdateTask
)

// SplitNamespace returns db, collection
func SplitNamespace(namespace string) (string, string) {
	dot := strings.Index(namespace, ".")
	if dot < 0 {
		return namespace, ""
	}
	return namespace[:dot], namespace[dot+1:]
}

// Refetch contains the data necessary to track a refretch
type Refetch struct {
	ID            interface{} `bson:"id"`
	SrcNamespace  string      `bson:"srcNamespace"`
	DestNamespace string      `bson:"destNamespace"`
	Status        string
}

// TaskError contains error Code and Message
type TaskError struct {
	Code    int
	Message string
}

func (e TaskError) Error() string {
	return e.Message
}

// QueryFilter stores namespace and query
type QueryFilter struct {
	Filter       bson.D   `json:"filter,omitempty" bson:"filter,omitempty"`
	Limit        int64    `json:"limit,omitempty" bson:"limit,omitempty"`
	Masks        []string `json:"masks,omitempty" bson:"masks,omitempty"`
	Method       string   `json:"method,omitempty" bson:"method,omitempty"`
	Namespace    string   `json:"namespace" bson:"namespace"`
	To           string   `json:"to,omitempty" bson:"to,omitempty"`
	ChannelCount int      `json:"numChan,omitempty" bson:"numChan,omitempty"`
}
