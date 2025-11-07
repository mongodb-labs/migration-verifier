package verifier

import (
	"encoding/binary"
	"fmt"

	"github.com/10gen/migration-verifier/mbson"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
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

var _ bson.Marshaler = VerificationResult{}
var _ bson.Unmarshaler = &VerificationResult{}

func (vr VerificationResult) MarshalBSON() ([]byte, error) {
	panic("Use MarshalToBSON.")
}

func (vr VerificationResult) MarshalToBSON() []byte {
	if vr.ID.IsZero() {
		vr.ID = bson.RawValue{Type: bson.TypeNull}
	}

	bsonLen := 4 + // header
		1 + 2 + 1 + // ID
		1 + 5 + 1 + 4 + 1 + // Field
		1 + 7 + 1 + 4 + 1 + // Details
		1 + 7 + 1 + 4 + 1 + // Cluster
		1 + 9 + 1 + 4 + 1 + // NameSpace
		1 // NUL

	bsonLen += len(vr.ID.Value) +
		len(vr.Field) +
		len(vr.Details) +
		len(vr.Cluster) +
		len(vr.NameSpace)

	if vr.SrcTimestamp.IsSome() {
		bsonLen += 1 + 12 + 1 + 8
	}

	if vr.DstTimestamp.IsSome() {
		bsonLen += 1 + 12 + 1 + 8
	}

	buf := make(bson.Raw, 4, bsonLen)

	binary.LittleEndian.PutUint32(buf, uint32(bsonLen))

	buf = bsoncore.AppendValueElement(buf, "id", bsoncore.Value{
		Type: bsoncore.Type(vr.ID.Type),
		Data: vr.ID.Value,
	})

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

	buf = append(buf, 0)

	if len(buf) != bsonLen {
		panic(fmt.Sprintf("%T BSON length is %d but expected %d", vr, len(buf), bsonLen))
	}

	return buf
}

func (vr *VerificationResult) UnmarshalBSON(in []byte) error {
	panic("Use UnmarshalFromBSON.")
}

func (vr *VerificationResult) UnmarshalFromBSON(in []byte) error {
	for el, err := range mbson.RawElements(bson.Raw(in)) {
		if err != nil {
			return errors.Wrap(err, "iterating BSON doc fields")
		}

		key, err := el.KeyErr()
		if err != nil {
			return errors.Wrap(err, "extracting BSON doc’s field name")
		}

		switch key {
		case "id":
			rv, err := el.ValueErr()
			if err != nil {
				return errors.Wrapf(err, "parsing %#q field", key)
			}

			vr.ID = rv
		case "field":
			if err := mbson.UnmarshalElementValue(el, &vr.Field); err != nil {
				return err
			}
		case "details":
			if err := mbson.UnmarshalElementValue(el, &vr.Details); err != nil {
				return err
			}
		case "cluster":
			if err := mbson.UnmarshalElementValue(el, &vr.Cluster); err != nil {
				return err
			}
		case "namespace":
			if err := mbson.UnmarshalElementValue(el, &vr.NameSpace); err != nil {
				return err
			}
		case "srctimestamp":
			rv, err := el.ValueErr()
			if err != nil {
				return errors.Wrapf(err, "parsing %#q field", key)
			}

			if rv.Type != bson.TypeNull {
				var ts bson.Timestamp
				if err := mbson.UnmarshalRawValue(rv, &ts); err != nil {
					return err
				}

				vr.SrcTimestamp = option.Some(ts)
			}

		case "dsttimestamp":
			rv, err := el.ValueErr()
			if err != nil {
				return errors.Wrapf(err, "parsing %#q field", key)
			}

			if rv.Type != bson.TypeNull {
				var ts bson.Timestamp
				if err := mbson.UnmarshalRawValue(rv, &ts); err != nil {
					return err
				}

				vr.DstTimestamp = option.Some(ts)
			}
		}
	}

	return nil
}
