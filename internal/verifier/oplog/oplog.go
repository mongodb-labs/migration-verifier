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

type Op struct {
	Op      string
	TS      bson.Timestamp
	Ns      string
	CmdName string
	DocLen  int32
	DocID   bson.RawValue
	Ops     []Op
}

type ResumeToken struct {
	TS bson.Timestamp
}

func GetRawResumeTokenTimestamp(token bson.Raw) (bson.Timestamp, error) {
	rv, err := token.LookupErr("ts")
	if err != nil {
		return bson.Timestamp{}, errors.Wrap(err, "getting ts")
	}

	return mbson.CastRawValue[bson.Timestamp](rv)
}

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

func (o *Op) UnmarshalFromBSON(in []byte) error {
	//fmt.Printf("---- unmarshaling: %+v\n\n", bson.Raw(in))

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
