package verifier

import (
	"fmt"

	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// ParsedEvent contains the fields of an event that we have parsed from 'bson.Raw'.
type ParsedEvent struct {
	OpType       string                         `bson:"operationType"`
	Ns           *Namespace                     `bson:"ns,omitempty"`
	DocID        bson.RawValue                  `bson:"_docID,omitempty"`
	FullDocument bson.Raw                       `bson:"fullDocument,omitempty"`
	FullDocLen   option.Option[types.ByteCount] `bson:"_fullDocLen"`
	ClusterTime  *primitive.Timestamp           `bson:"clusterTime,omitEmpty"`
}

func (pe *ParsedEvent) String() string {
	return fmt.Sprintf("{OpType: %s, namespace: %s, docID: %v, clusterTime: %v}", pe.OpType, pe.Ns, pe.DocID, pe.ClusterTime)
}

var _ bson.Unmarshaler = &ParsedEvent{}

// UnmarshalBSON implements bson.Unmarshaler. We define this manually to
// avoid reflection, which can substantially impede performance in “hot”
// code paths like this.
func (pe *ParsedEvent) UnmarshalBSON(in []byte) error {
	els, err := bson.Raw(in).Elements()
	if err != nil {
		return errors.Wrapf(err, "parsing elements")
	}

	for _, el := range els {
		key, err := el.KeyErr()
		if err != nil {
			return errors.Wrapf(err, "parsing field name")
		}

		var rv bson.RawValue

		switch key {
		case "operationType":
			rv, err = el.ValueErr()
			if err == nil {
				err = mbson.UnmarshalRawValue(rv, &pe.OpType)
			}
		case "ns":
			rv, err = el.ValueErr()
			if err == nil {
				var rvDoc bson.Raw
				err = mbson.UnmarshalRawValue(rv, &rvDoc)

				if err == nil {
					ns := Namespace{}

					// Here we might as well call UnmarshalBSON directly
					// rather than calling bson.Unmarshal.
					err = ns.UnmarshalBSON(rvDoc)

					if err == nil {
						pe.Ns = &ns
					}
				}
			}
		case "_docID":
			rv, err = el.ValueErr()

			if err == nil {
				pe.DocID = rv
			}
		case "fullDocument":
			rv, err = el.ValueErr()

			if err == nil {
				err = mbson.UnmarshalRawValue(rv, &pe.FullDocument)
			}
		case "_fullDocLen":
			rv, err = el.ValueErr()

			if err == nil && rv.Type != bson.TypeNull {
				docLen, ok := rv.AsInt64OK()

				if !ok {
					err = fmt.Errorf("BSON type %s (value: %v) cannot be int64", rv.Type, rv)
				} else {
					pe.FullDocLen = option.Some(types.ByteCount(docLen))
				}
			}
		case "clusterTime":
			rv, err = el.ValueErr()
			if err == nil {
				var ct primitive.Timestamp
				err = mbson.UnmarshalRawValue(rv, &ct)

				if err == nil {
					pe.ClusterTime = &ct
				}
			}
		}

		if err != nil {
			return errors.Wrapf(err, "parsing %#q", key)
		}
	}

	return nil
}
