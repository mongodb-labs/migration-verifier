package compare

import (
	"fmt"
	"slices"

	"github.com/10gen/migration-verifier/dockey"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type Method string

const (
	Binary           Method = "binary"
	IgnoreOrder      Method = "ignoreOrder"
	ToHashedIndexKey Method = "toHashedIndexKey"

	// When comparing documents via hash, we store the document key as an
	// embedded document. This is the name of the field that stores the
	// document key.
	docKeyInHashedCompare = "k"
)

var (
	Methods = mslices.Of(
		Binary,
		IgnoreOrder,
		ToHashedIndexKey,
	)

	Default = Methods[0]
)

func (dcm Method) ShouldIgnoreFieldOrder() bool {
	return dcm == IgnoreOrder
}

func (dcm Method) ComparesFullDocuments() bool {
	return dcm == Binary || dcm == IgnoreOrder
}

func (dcm Method) ClonedDocIDForComparison(doc bson.Raw) (bson.RawValue, error) {
	docID, err := dcm.RawDocIDForComparison(doc)
	if err != nil {
		return bson.RawValue{}, err
	}

	// We clone the value because the document might be from a pool.
	docID.Value = slices.Clone(docID.Value)

	return docID, nil
}

func (dcm Method) RawDocIDForComparison(
	doc bson.Raw,
) (bson.RawValue, error) {
	switch dcm {
	case Binary, IgnoreOrder:
		return doc.LookupErr("_id")
	case ToHashedIndexKey:
		return doc.LookupErr(docKeyInHashedCompare, "_id")
	default:
		return bson.RawValue{}, fmt.Errorf("unknown doc compare method: %#q", dcm)
	}
}

func (dcm Method) GetDocKeyValues(
	doc bson.Raw,
	fieldNames []string,
) ([]bson.RawValue, error) {
	var docKey bson.Raw

	switch dcm {
	case Binary, IgnoreOrder:
		// If we have the full document, create the document key manually:
		var err error
		docKey, err = dockey.ExtractTrueDocKeyFromDoc(fieldNames, doc)
		if err != nil {
			return nil, err
		}
	case ToHashedIndexKey:
		// If we have a hash, then the aggregation should have extracted the
		// document key for us.
		docKeyVal, err := doc.LookupErr(docKeyInHashedCompare)
		if err != nil {
			return nil, errors.Wrapf(err, "fetching %#q from doc %v", docKeyInHashedCompare, doc)
		}

		var isDoc bool
		docKey, isDoc = docKeyVal.DocumentOK()
		if !isDoc {
			return nil, fmt.Errorf(
				"%#q in doc %v is type %s but should be %s",
				docKeyInHashedCompare,
				doc,
				docKeyVal.Type,
				bson.TypeEmbeddedDocument,
			)
		}
	}

	var values []bson.RawValue
	els, err := docKey.Elements()
	if err != nil {
		return nil, errors.Wrapf(err, "parsing doc key (%+v) of doc %+v", docKey, doc)
	}

	for _, el := range els {
		val, err := el.ValueErr()
		if err != nil {
			return nil, errors.Wrapf(err, "parsing doc key element (%+v) of doc %+v", el, doc)
		}

		values = append(values, val)
	}

	return values, nil
}

func GetHashedIndexKeyProjection(qf tasks.QueryFilter) bson.D {
	return bson.D{
		{"_id", "$$REMOVE"},

		// Single-letter field names minimize the document size.
		{docKeyInHashedCompare, dockey.ExtractTrueDocKeyAgg(
			qf.GetDocKeyFields(),
			"$$ROOT",
		)},
		{"h", bson.D{
			{"$toHashedIndexKey", bson.D{
				{"$_internalKeyStringValue", bson.D{
					{"input", "$$ROOT"},
				}},
			}},
		}},
		{"s", bson.D{{"$bsonSize", "$$ROOT"}}},
	}
}
