package recheck

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/10gen/migration-verifier/mbson"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/x/bsonx/bsoncore"
)

// PrimaryKey stores the implicit type of recheck to perform
// Currently, we only handle document mismatches/change stream updates,
// so SrcDatabaseName, SrcCollectionName, and DocumentID must always be specified.
//
// NB: Order is important here so that, within a given generation,
// sorting by _id will guarantee that all rechecks for a given
// namespace appear consecutively.
type PrimaryKey struct {
	SrcDatabaseName   string        `bson:"db"`
	SrcCollectionName string        `bson:"coll"`
	DocumentID        bson.RawValue `bson:"docID"`

	// Rand is here to allow “duplicate” entries. We do this because, with
	// multiple change streams returning the same events, we expect duplicate
	// key errors to be frequent. The server is quite slow in handling such
	// errors, though. To avoid that, while still allowing the _id index to
	// facilitate easy sorting of the duplicates, we set this field to a
	// random value on each entry.
	//
	// This also avoids duplicate-key slowness where the source workload
	// involves frequent writes to a small number of documents.
	Rand int32
}

var _ bson.Marshaler = PrimaryKey{}
var _ bson.Unmarshaler = &PrimaryKey{}

// MarshalBSON implements bson.Marshaler .. which is only done to prevent
// the inefficiency of bson.Marshal().
func (pk PrimaryKey) MarshalBSON() ([]byte, error) {
	panic("Use MarshalToBSON instead.")
}

func (pk PrimaryKey) MarshalToBSON() []byte {
	// This is a very “hot” path, so we want to minimize allocations.
	variableSize := len(pk.SrcDatabaseName) + len(pk.SrcCollectionName) + len(pk.DocumentID.Value)

	// This document’s nonvariable parts:
	expectedLen := 42 + variableSize

	doc := make(bson.Raw, 4, expectedLen)

	doc = bsoncore.AppendStringElement(doc, "db", pk.SrcDatabaseName)
	doc = bsoncore.AppendStringElement(doc, "coll", pk.SrcCollectionName)
	doc = bsoncore.AppendValueElement(doc, "docID", bsoncore.Value{
		Type: bsoncore.Type(pk.DocumentID.Type),
		Data: pk.DocumentID.Value,
	})
	doc = bsoncore.AppendInt32Element(doc, "rand", pk.Rand)

	doc = append(doc, 0)

	if len(doc) != expectedLen {
		panic(fmt.Sprintf("Unexpected %T BSON size %d; expected %d", pk, len(doc), expectedLen))
	}

	binary.LittleEndian.PutUint32(doc, uint32(len(doc)))

	return doc
}

func (pk *PrimaryKey) UnmarshalBSON(in []byte) error {
	panic("Use UnmarshalFromBSON instead!")
}

func (pk *PrimaryKey) UnmarshalFromBSON(in []byte) error {
	for el, err := range mbson.RawElements(bson.Raw(in)) {
		if err != nil {
			return errors.Wrap(err, "iterating BSON doc fields")
		}

		key, err := el.KeyErr()
		if err != nil {
			return errors.Wrap(err, "extracting BSON doc’s field name")
		}

		switch key {
		case "db":
			if err := mbson.UnmarshalElementValue(el, &pk.SrcDatabaseName); err != nil {
				return err
			}
		case "coll":
			if err := mbson.UnmarshalElementValue(el, &pk.SrcCollectionName); err != nil {
				return err
			}
		case "docID":
			rv, err := el.ValueErr()
			if err != nil {
				return errors.Wrapf(err, "parsing %#q field", key)
			}

			pk.DocumentID = rv
		case "rand":
			if err := mbson.UnmarshalElementValue(el, &pk.Rand); err != nil {
				return err
			}
		}
	}

	return nil
}

type MismatchTimes struct {
	First  time.Time
	Latest time.Time
}

var MismatchTimesBSONLength = len(MismatchTimes{}.MarshalToBSON())

// NB: We allow bson.Unmarshal to work because it’s needed in VerificationTask.
var _ bson.Marshaler = MismatchTimes{}
var _ bson.Unmarshaler = &MismatchTimes{}

func (mt MismatchTimes) MarshalBSON() ([]byte, error) {
	return mt.MarshalToBSON(), nil
}

func (mt *MismatchTimes) UnmarshalBSON(in []byte) error {
	return mt.UnmarshalFromBSON(in)
}

func (mt MismatchTimes) MarshalToBSON() []byte {
	expectedLen := 4 + // header
		1 + 5 + 1 + 8 + // first
		1 + 6 + 1 + 8 + //latest
		0

	doc := make(bson.Raw, 4, expectedLen)
	binary.LittleEndian.PutUint32(doc, uint32(len(doc)))

	doc = bsoncore.AppendDateTimeElement(doc, "first", mt.First.UnixMilli())
	doc = bsoncore.AppendDateTimeElement(doc, "latest", mt.Latest.UnixMilli())
	doc = append(doc, 0)

	if len(doc) != expectedLen {
		panic(fmt.Sprintf("Unexpected %T BSON size %d; expected %d", mt, len(doc), expectedLen))
	}

	return doc
}

func (mt *MismatchTimes) UnmarshalFromBSON(in []byte) error {
	for el, err := range mbson.RawElements(bson.Raw(in)) {
		if err != nil {
			return errors.Wrap(err, "iterating BSON doc fields")
		}

		key, err := el.KeyErr()
		if err != nil {
			return errors.Wrap(err, "extracting BSON doc’s field name")
		}

		switch key {
		case "first":
			if err = mbson.UnmarshalElementValue(el, &mt.First); err != nil {
				return err
			}
		case "latest":
			if err = mbson.UnmarshalElementValue(el, &mt.Latest); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unmarshaling to %T: unknown BSON field %#q", *mt, key)
		}
	}

	return nil
}

// Doc stores the necessary information to know which documents must be rechecked.
type Doc struct {
	PrimaryKey PrimaryKey `bson:"_id"`

	// NB: Because we don’t update the recheck queue’s documents, this field
	// and any others that may be added will remain unchanged even if a recheck
	// is enqueued multiple times for the same document in the same generation.
	DataSize int32 `bson:"dataSize"`

	// Mismatch tracks the times when this doc was seen mismatched.
	Mismatch option.Option[MismatchTimes] `bson:"mismatch"`
}

var _ bson.Marshaler = Doc{}
var _ bson.Unmarshaler = &Doc{}

// MarshalBSON implements bson.Marshaler .. which is only done to prevent
// the inefficiency of bson.Marshal().
func (rd Doc) MarshalBSON() ([]byte, error) {
	panic("Use MarshalToBSON instead.")
}

func (rd Doc) MarshalToBSON() []byte {
	keyRaw := rd.PrimaryKey.MarshalToBSON()

	// This document’s nonvariable parts comprise 24 bytes.
	expectedLen := 24 + len(keyRaw)

	doc := make(bson.Raw, 4, expectedLen)
	doc = bsoncore.AppendDocumentElement(doc, "_id", keyRaw)
	doc = bsoncore.AppendInt32Element(doc, "dataSize", int32(rd.DataSize))

	doc = append(doc, 0)

	if len(doc) != expectedLen {
		panic(fmt.Sprintf("Unexpected %T BSON size %d; expected %d", rd, len(doc), expectedLen))
	}

	binary.LittleEndian.PutUint32(doc, uint32(len(doc)))

	return doc
}

func (rd *Doc) UnmarshalBSON(in []byte) error {
	panic("Use UnmarshalFromBSON instead!")
}

func (rd *Doc) UnmarshalFromBSON(in []byte) error {
	for el, err := range mbson.RawElements(bson.Raw(in)) {
		if err != nil {
			return errors.Wrap(err, "iterating BSON doc fields")
		}

		key, err := el.KeyErr()
		if err != nil {
			return errors.Wrap(err, "extracting BSON doc’s field name")
		}

		switch key {
		case "_id":
			var rvDoc bson.Raw
			if err := mbson.UnmarshalElementValue(el, &rvDoc); err != nil {
				return err
			}

			if err := (&rd.PrimaryKey).UnmarshalFromBSON(rvDoc); err != nil {
				return err
			}
		case "dataSize":
			if err := mbson.UnmarshalElementValue(el, &rd.DataSize); err != nil {
				return err
			}
		case "mismatch":
			elType := bson.Type(el[0])
			if elType != bson.TypeEmbeddedDocument {
				return fmt.Errorf(
					"%#q should be BSON %s but found %s",
					key,
					bson.TypeEmbeddedDocument,
					elType,
				)
			}

			var mmTimes MismatchTimes
			if err := mmTimes.UnmarshalFromBSON(el.Value().Document()); err != nil {
				return errors.Wrapf(err, "unmarshaling %#q", key)
			}

			rd.Mismatch = option.Some(mmTimes)
		}
	}

	return nil
}
