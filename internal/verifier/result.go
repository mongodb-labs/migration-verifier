package verifier

import (
	"encoding/binary"
	"fmt"

	"github.com/10gen/migration-verifier/internal/verifier/recheck"
	"github.com/10gen/migration-verifier/option"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/x/bsonx/bsoncore"
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

	// The number of generations where we’ve seen this document ID mismatched
	// without a change event.
	MismatchTimes option.Option[recheck.MismatchTimes] `bson:"mismatchTimes,omitempty"`
}

// DocumentIsMissing returns a boolean that indicates whether the
// VerificationResult indicates a document that is missing on either
// source or destination.
func (vr VerificationResult) DocumentIsMissing() bool {
	// NB: Missing gets set as the Details value when a field is missing
	// but the document exists. To ascertain that the document is entirely
	// absent we have to check Field as well.
	return vr.Details == Missing && vr.Field == ""
}

// Returns an agg expression that indicates whether the VerificationResult
// refers to a missing document.
func getResultDocMissingAggExpr(docExpr any) bson.D {
	return bson.D{
		{"$and", []bson.D{
			{{"$eq", bson.A{
				Missing,
				bson.D{{"$getField", bson.D{
					{"input", docExpr},
					{"field", "details"},
				}}},
			}}},
			{{"$eq", bson.A{
				"",
				bson.D{{"$getField", bson.D{
					{"input", docExpr},
					{"field", "field"},
				}}},
			}}},
		}},
	}
}

var _ bson.Marshaler = VerificationResult{}

func (vr VerificationResult) MarshalBSON() ([]byte, error) {
	panic("Use MarshalToBSON.")
}

func (vr VerificationResult) MarshalToBSON() []byte {
	bsonLen := 4 + // header
		1 + 5 + 1 + 4 + 1 + // Field
		1 + 7 + 1 + 4 + 1 + // Details
		1 + 7 + 1 + 4 + 1 + // Cluster
		1 + 9 + 1 + 4 + 1 + // NameSpace
		1 // NUL

	bsonLen += 0 +
		len(vr.Field) +
		len(vr.Details) +
		len(vr.Cluster) +
		len(vr.NameSpace)

	if !vr.ID.IsZero() {
		bsonLen += 1 + 2 + 1 + len(vr.ID.Value)
	}

	if vr.SrcTimestamp.IsSome() {
		bsonLen += 1 + 12 + 1 + 8
	}

	if vr.DstTimestamp.IsSome() {
		bsonLen += 1 + 12 + 1 + 8
	}

	if vr.MismatchTimes.IsSome() {
		bsonLen += 1 + 13 + 1 + recheck.MismatchTimesBSONLength
	}

	buf := make(bson.Raw, 4, bsonLen)

	binary.LittleEndian.PutUint32(buf, uint32(bsonLen))

	if !vr.ID.IsZero() {
		buf = bsoncore.AppendValueElement(buf, "id", bsoncore.Value{
			Type: bsoncore.Type(vr.ID.Type),
			Data: vr.ID.Value,
		})
	}

	buf = bsoncore.AppendStringElement(buf, "field", vr.Field)
	buf = bsoncore.AppendStringElement(buf, "details", vr.Details)
	buf = bsoncore.AppendStringElement(buf, "cluster", vr.Cluster)
	buf = bsoncore.AppendStringElement(buf, "namespace", vr.NameSpace)

	if ts, has := vr.SrcTimestamp.Get(); has {
		buf = bsoncore.AppendTimestampElement(buf, "srctimestamp", ts.T, ts.I)
	}

	if ts, has := vr.DstTimestamp.Get(); has {
		buf = bsoncore.AppendTimestampElement(buf, "dsttimestamp", ts.T, ts.I)
	}

	if mm, has := vr.MismatchTimes.Get(); has {
		buf = bsoncore.AppendDocumentElement(buf, "mismatchTimes", mm.MarshalToBSON())
	}

	buf = append(buf, 0)

	if len(buf) != bsonLen {
		panic(fmt.Sprintf("%T BSON length is %d but expected %d", vr, len(buf), bsonLen))
	}

	return buf
}
