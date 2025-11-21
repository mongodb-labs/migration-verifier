package oplog

import (
	"encoding/binary"
	"fmt"
	"slices"

	"github.com/10gen/migration-verifier/mbson"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/x/bsonx/bsoncore"
)

const (
	rtBSONLength = 4 + 1 + 2 + 1 + 8 + 1
)

// Op is an internal representation of the parts of an oplog entry that we
// care about. This struct is not meant to be marshaled/unmarshaled directly.
type Op struct {
	// Op holds the oplog entry’s `op`.
	Op string

	// TS holds the oplog entry’s `ts`.
	TS bson.Timestamp

	// Ns holds the oplog entry’s `ns`.
	Ns string

	// CmdName is the first field name in the oplog entry’s `o` document.
	CmdName string

	// DocLen is the length, in bytes, of whatever document the oplog entry
	// describes. This will only be meaningful for insert & replace entries.
	DocLen int32

	// DocID is the `_id` of whatever document the oplog entry describes.
	// This won’t be populated for multi-op Op instances.
	DocID bson.RawValue

	// Ops holds the ops in an `applyOps` oplog entry.
	Ops []Op
}

func (*Op) UnmarshalBSON([]byte) error {
	panic("Use UnmarshalFromBSON.")
}

// UnmarshalFromBSON unmarshals an Op more efficiently than the standard
// bson.Unmarshal function. This function is called for every oplog entry,
// so that efficiency is material.
func (o *Op) UnmarshalFromBSON(in []byte) error {
	for el, err := range mbson.RawElements(bson.Raw(in)) {
		if err != nil {
			return errors.Wrap(err, "iterating BSON document")
		}

		key, err := el.KeyErr()
		if err != nil {
			return errors.Wrap(err, "reading BSON field name")
		}

		switch key {
		case "op":
			err = mbson.UnmarshalElementValue(el, &o.Op)
		case "ts":
			err = mbson.UnmarshalElementValue(el, &o.TS)
		case "ns":
			err = mbson.UnmarshalElementValue(el, &o.Ns)
		case "cmdName":
			err = mbson.UnmarshalElementValue(el, &o.CmdName)
		case "docLen":
			err = mbson.UnmarshalElementValue(el, &o.DocLen)
		case "docID":
			o.DocID, err = el.ValueErr()
			if err != nil {
				err = errors.Wrapf(err, "parsing %#q value", key)
			}
			o.DocID.Value = slices.Clone(o.DocID.Value)
		case "ops":
			var arr bson.RawArray
			err = errors.Wrapf(
				mbson.UnmarshalElementValue(el, &arr),
				"parsing ops",
			)

			if err == nil {
				vals, err := arr.Values()
				if err != nil {
					return errors.Wrap(err, "parsing applyOps")
				}

				o.Ops = make([]Op, len(vals))

				for i, val := range vals {

					var opRaw bson.Raw
					err := mbson.UnmarshalRawValue(val, &opRaw)
					if err != nil {
						return errors.Wrapf(err, "parsing applyOps field")
					}

					if err := (&o.Ops[i]).UnmarshalFromBSON(opRaw); err != nil {
						return errors.Wrapf(err, "parsing applyOps value")
					}
				}
			}
		default:
			err = errors.Wrapf(err, "unexpected field %#q", key)
		}

		if err != nil {
			return err
		}
	}

	return nil
}

// ResumeToken is Migration Verifier’s internal format for storing the
// timestamp to resume an oplog reader.
type ResumeToken struct {
	TS bson.Timestamp
}

func (ResumeToken) MarshalBSON() ([]byte, error) {
	panic("Use MarshalToBSON.")
}

// MarshalToBSON marshals a ResumeToken to BSON. Unlike with the standard
// bson.Marshaler interface, this method never fails. It’s also faster/lighter
// because it avoids reflection, which is relevant because this is called for
// every batch of ops.
func (rt ResumeToken) MarshalToBSON() []byte {
	buf := make([]byte, 4, rtBSONLength)

	binary.LittleEndian.PutUint32(buf, uint32(cap(buf)))

	buf = bsoncore.AppendTimestampElement(buf, "ts", rt.TS.T, rt.TS.I)

	buf = append(buf, 0)

	if len(buf) != rtBSONLength {
		panic(fmt.Sprintf("bad resume token BSON length: %d", len(buf)))
	}

	return buf
}

// GetRawResumeTokenTimestamp extracts the timestamp from a given oplog entry.
func GetRawResumeTokenTimestamp(token bson.Raw) (bson.Timestamp, error) {
	rv, err := token.LookupErr("ts")
	if err != nil {
		return bson.Timestamp{}, errors.Wrap(err, "getting ts")
	}

	return mbson.CastRawValue[bson.Timestamp](rv)
}
