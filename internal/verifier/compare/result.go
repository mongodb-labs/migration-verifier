package compare

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/10gen/migration-verifier/internal/verifier/api"
	"github.com/10gen/migration-verifier/internal/verifier/constants"
	"github.com/10gen/migration-verifier/internal/verifier/recheck"
	"github.com/10gen/migration-verifier/option"
	"github.com/mongodb-labs/migration-tools/bsontools"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/x/bsonx/bsoncore"
)

const (
	Missing = "Missing"
)

// Result holds the result of a single comparison.
type Result struct {
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

	// The number of generations where we’ve seen this document ID mismatched
	// without a change event.
	MismatchHistory recheck.MismatchHistory `bson:"mismatchHistory,omitempty"`

	// The data size of the largest of the mismatched objects.
	// Note this is used only to ensure recheck tasks’ queries don’t return
	// too many results.
	DataSize int32

	SrcTimestamp option.Option[bson.Timestamp]
	DstTimestamp option.Option[bson.Timestamp]
}

// DocumentIsMissing returns a boolean that indicates whether the
// VerificationResult indicates a document that is missing on either
// source or destination.
func (vr Result) DocumentIsMissing() bool {
	// NB: Missing gets set as the Details value when a field is missing
	// but the document exists. To ascertain that the document is entirely
	// absent we have to check Field as well.
	return vr.Details == Missing && vr.Field == ""
}

func (vr Result) APINSMismatchInfo() api.NSMismatchInfo {
	apiMM := api.NSMismatchInfo{
		Namespace: vr.NameSpace,
		Detail:    option.IfNotZero(vr.Details),
	}

	if vr.Details == Missing {
		apiMM.Type = lo.Ternary(
			vr.Cluster == constants.ClusterSource,
			api.MismatchExtra,
			api.MismatchMissing,
		)

		apiMM.Detail = option.None[string]()
	} else {
		apiMM.Type = api.MismatchContent
	}

	if vr.ID.Equal(bsontools.ToRawValue("spec")) {
		apiMM.Aspect = api.NSMismatchAspectSpec
		apiMM.Component = option.Some(vr.Field)
	} else {
		switch vr.Field {
		case "":
			apiMM.Aspect = api.NSMismatchAspectExist

			lo.Assertf(
				vr.Details == Missing,
				"exist ns mismatch but have details (%s)",
				vr.Details,
			)
		case "type":
			apiMM.Aspect = api.NSMismatchAspectType
		case "shard key":
			apiMM.Aspect = api.NSMismatchAspectShardKey
		case "index":
			apiMM.Aspect = api.NSMismatchAspectIndex

			indexName, err := bsontools.RawValueToString(vr.ID)
			lo.Assertf(
				err == nil,
				"extracting _id: %v",
				err,
			)

			apiMM.Component = option.Some(indexName)

		// This shouldn’t happen. It’s only here for completeness since
		// Verifier checks for it internally.
		case "readOnly":
			apiMM.Aspect = api.NSMismatchAspectReadOnly

		default:
			lo.Assertf(false, "unexpected mismatch field: %#q", vr.Field)
		}
	}

	return apiMM
}

func (vr Result) APIDocMismatchInfo() api.DocMismatchInfo {
	apiMM := api.DocMismatchInfo{
		ID:           vr.ID,
		Namespace:    vr.NameSpace,
		DurationSecs: vr.MismatchDuration().Seconds(),
	}

	if vr.DocumentIsMissing() {
		apiMM.Type = lo.Ternary(
			vr.Cluster == constants.ClusterSource,
			api.MismatchExtra,
			api.MismatchMissing,
		)
	} else {
		apiMM.Type = api.MismatchContent
		apiMM.Detail = option.Some(vr.Details)
	}

	// NB: In hashed comparison we don’t know the field name.
	// Also, with collection-level mismatches the Field indicates the
	// mismatch “subtype” (e.g., index, …)
	apiMM.Field = option.IfNotZero(vr.Field)

	return apiMM
}

func (vr Result) MismatchDuration() time.Duration {
	return time.Duration(vr.MismatchHistory.DurationMS) * time.Millisecond
}

var _ bson.Marshaler = Result{}

func (vr Result) MarshalBSON() ([]byte, error) {
	panic("Use MarshalToBSON.")
}

func (vr Result) MarshalToBSON() []byte {
	bsonLen := 4 + // header
		1 + 5 + 1 + 4 + 1 + // Field
		1 + 7 + 1 + 4 + 1 + // Details
		1 + 7 + 1 + 4 + 1 + // Cluster
		1 + 9 + 1 + 4 + 1 + // NameSpace
		1 + 15 + 1 + recheck.MismatchHistoryBSONLength +
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
	buf = bsoncore.AppendDocumentElement(buf, "mismatchHistory", vr.MismatchHistory.MarshalToBSON())

	if ts, has := vr.SrcTimestamp.Get(); has {
		buf = bsoncore.AppendTimestampElement(buf, "srctimestamp", ts.T, ts.I)
	}

	if ts, has := vr.DstTimestamp.Get(); has {
		buf = bsoncore.AppendTimestampElement(buf, "dsttimestamp", ts.T, ts.I)
	}

	buf = append(buf, 0)

	if len(buf) != bsonLen {
		panic(fmt.Sprintf("%T BSON length is %d but expected %d", vr, len(buf), bsonLen))
	}

	return buf
}
