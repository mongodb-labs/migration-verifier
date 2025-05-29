package verifier

import (
	"github.com/10gen/migration-verifier/option"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

const (
	Missing = "Missing"
)

// VerificationResult holds the Verification Results.
type VerificationResult struct {

	// This field gets used differently depending on whether this result
	// came from a document comparison or something else. If it’s from a
	// document comparison, it *MUST* be a document ID, not a
	// documentmap.MapKey, because we query on this to populate verification
	// tasks for rechecking after a document mismatch. Thus, in sharded
	// clusters with duplicate document IDs in the same collection, multiple
	// VerificationResult instances might share the same ID. That’s OK,
	// though; it’ll just make the recheck include all docs with that ID,
	// regardless of which ones actually need the recheck.
	ID any

	Field     string
	Details   string
	Cluster   string
	NameSpace string
	// The data size of the largest of the mismatched objects.
	// Note this is not persisted; it is used only to ensure recheck tasks
	// don't get too large.
	dataSize int

	SrcTimestamp option.Option[primitive.Timestamp]
	DstTimestamp option.Option[primitive.Timestamp]
}

func (vr VerificationResult) IsMissing() bool {
	return vr.Details == Missing
}
