package verifier

import (
	"fmt"

	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	// The number of batches we’ll hold in memory at once.
	batchChanBufferSize = 100
)

type changeEventBatch struct {
	events      []ParsedEvent
	resumeToken bson.Raw
	clusterTime bson.Timestamp
}

// ParsedEvent contains the fields of an event that we have parsed from 'bson.Raw'.
type ParsedEvent struct {
	OpType       string                         `bson:"operationType"`
	Ns           *Namespace                     `bson:"ns,omitempty"`
	DocID        bson.RawValue                  `bson:"_docID,omitempty"`
	FullDocument bson.Raw                       `bson:"fullDocument,omitempty"`
	FullDocLen   option.Option[types.ByteCount] `bson:"_fullDocLen"`
	ClusterTime  *bson.Timestamp                `bson:"clusterTime,omitEmpty"`
}

func (pe *ParsedEvent) String() string {
	return fmt.Sprintf("{OpType: %s, namespace: %s, docID: %v, clusterTime: %v}", pe.OpType, pe.Ns, pe.DocID, pe.ClusterTime)
}

var _ bson.Unmarshaler = &ParsedEvent{}

// UnmarshalBSON implements bson.Unmarshaler but panics because
// it’s faster to use UnmarshalFromBSON than bson.Unmarshal().
func (pe *ParsedEvent) UnmarshalBSON(in []byte) error {
	panic("Use UnmarshalFromBSON instead.")
}

// UnmarshalFromBSON unmarshals from BSON without the overhead of
// bson.Unmarshal.
func (pe *ParsedEvent) UnmarshalFromBSON(in []byte) error {
	for el, err := range mbson.RawElements(in) {
		if err != nil {
			return errors.Wrapf(err, "parsing elements")
		}

		key, err := el.KeyErr()
		if err != nil {
			return errors.Wrapf(err, "parsing field name")
		}

		var rv bson.RawValue

		switch key {
		case "operationType":
			err := mbson.UnmarshalElementValue(el, &pe.OpType)
			if err != nil {
				return err
			}

		case "ns":
			var rvDoc bson.Raw
			err := mbson.UnmarshalElementValue(el, &rvDoc)
			if err != nil {
				return err
			}

			ns := Namespace{}

			err = (&ns).UnmarshalFromBSON(rvDoc)
			if err != nil {
				return errors.Wrapf(err, "unmarshaling %#q value", key)
			}

			pe.Ns = &ns
		case "_docID":
			rv, err = el.ValueErr()
			if err != nil {
				return errors.Wrapf(err, "parsing %#q field", key)
			}

			pe.DocID = rv
		case "fullDocument":
			err := mbson.UnmarshalElementValue(el, &pe.FullDocument)
			if err != nil {
				return err
			}
		case "_fullDocLen":
			rv, err = el.ValueErr()
			if err != nil {
				return errors.Wrapf(err, "parsing %#q field", key)
			}

			if rv.Type != bson.TypeNull {
				docLen, ok := rv.AsInt64OK()

				if !ok {
					return fmt.Errorf("%#q BSON type %s (value: %v) cannot be %T", key, rv.Type, rv, docLen)
				} else {
					pe.FullDocLen = option.Some(types.ByteCount(docLen))
				}
			}
		case "clusterTime":
			var ct bson.Timestamp
			err := mbson.UnmarshalElementValue(el, &ct)
			if err != nil {
				return err
			}

			pe.ClusterTime = &ct
		}
	}

	return nil
}
