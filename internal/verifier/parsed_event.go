package verifier

import (
	"fmt"

	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"github.com/samber/lo"
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
				rvDoc, err = mbson.CastRawValue[bson.Raw](rv)
				if err == nil {
					pe.Ns = &Namespace{
						DB:   rvDoc.Lookup("db").StringValue(),   // TODO
						Coll: rvDoc.Lookup("coll").StringValue(), // TODO
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
				pe.FullDocument = rv.Document() // TODO
			}
		case "_fullDocLen":
			rv, err = el.ValueErr()
			if err == nil && rv.Type != bson.TypeNull {
				pe.FullDocLen = option.Some(types.ByteCount(rv.AsInt64()))
			}
		case "clusterTime":
			rv, err = el.ValueErr()
			if err == nil {
				pe.ClusterTime = lo.ToPtr(lo.Must(mbson.CastRawValue[primitive.Timestamp](rv)))
			}
		}

		if err != nil {
			return errors.Wrapf(err, "parsing %#q", key)
		}
	}

	return nil
}
