package verifier

import (
	"github.com/10gen/migration-verifier/option"
	"go.mongodb.org/mongo-driver/v2/bson"
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
	ID bson.RawValue `bson:",omitempty"`

	Field     string
	Details   string
	Cluster   string
	NameSpace string

	// The data size of the largest of the mismatched objects.
	// Note this is not persisted; it is used only to ensure recheck tasks
	// don't get too large.
	dataSize int32

	SrcTimestamp option.Option[bson.Timestamp]
	DstTimestamp option.Option[bson.Timestamp]
}

/*
var _ bson.Marshaler = VerificationResult{}

func (vr VerificationResult) MarshalBSON() ([]byte, error) {
	panic("Use MarshalToBSON instead.")
}

func (vr VerificationResult) MarshalToBSON() ([]byte, error) {
	varSize := len(vr.ID.Value) +
		len(vr.Field) +
		len(vr.Details) +
		len(vr.Cluster) +
		len(vr.NameSpace)

	if vr.SrcTimestamp.IsSome() {
		varSize += 8
	}

	if vr.DstTimestamp.IsSome() {
		varSize += 8
	}

	expectedSize := 4 + // BSON header
		7 * 2 + // each value’s type & field name’s NUL
		4 * 4 + 4 + // 4 strings, each with a leading int32 & trailing NUL
}
*/

// DocumentIsMissing returns a boolean that indicates whether the
// VerificationResult indicates a document that is missing on either
// source or destination.
func (vr VerificationResult) DocumentIsMissing() bool {
	// NB: Missing gets set as the Details value when a field is missing
	// but the document exists. To ascertain that the document is entirely
	// absent we have to check Field as well.
	return vr.Details == Missing && vr.Field == ""
}
