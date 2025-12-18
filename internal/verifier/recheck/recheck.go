package recheck

import (
	"encoding/binary"
	"fmt"

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

// MismatchHistory records historical numbers on a mismatch.
type MismatchHistory struct {
	First      bson.DateTime
	DurationMS int64
}

var MismatchHistoryBSONLength = len(MismatchHistory{}.MarshalToBSON())

var _ bson.Marshaler = MismatchHistory{}
var _ bson.Unmarshaler = &MismatchHistory{}

func (mt MismatchHistory) MarshalBSON() ([]byte, error) {
	panic("Prefer MarshalToBSON.")
}

func (mt *MismatchHistory) UnmarshalBSON(in []byte) error {
	// We need this to work because VerificationResult doesn’t have
	// UnmarshalFromBSON.
	return mt.UnmarshalFromBSON(in)
}

func (mt MismatchHistory) MarshalToBSON() []byte {
	expectedLen := 4 + // header
		1 + 5 + 1 + 8 + // first
		1 + 10 + 1 + 8 + // duration
		1

	doc := make(bson.Raw, 4, expectedLen)
	binary.LittleEndian.PutUint32(doc, uint32(cap(doc)))

	doc = bsoncore.AppendDateTimeElement(doc, "first", int64(mt.First))
	doc = bsoncore.AppendInt64Element(doc, "durationMS", mt.DurationMS)
	doc = append(doc, 0)

	if len(doc) != expectedLen {
		panic(fmt.Sprintf("Unexpected %T BSON size %d; expected %d", mt, len(doc), expectedLen))
	}

	return doc
}

func (mt *MismatchHistory) UnmarshalFromBSON(in []byte) error {
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
		case "durationMS":
			if err = mbson.UnmarshalElementValue(el, &mt.DurationMS); err != nil {
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

	ChangeOpTime option.Option[bson.Timestamp]
	FromDst      bool `bson:",omitempty"`

	// FirstMismatchTime records when this doc was seen mismatched.
	FirstMismatchTime option.Option[bson.DateTime] `bson:"firstMismatchTime"`
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

	if rd.ChangeOpTime.IsSome() {
		expectedLen += 1 + len("changeOpTime") + 1 + 8
	} else if rd.FromDst {
		panic("Can’t be from destination without an optime!")
	}

	if rd.FromDst {
		expectedLen += 1 + len("fromDst") + 1 + 1
	}

	if rd.FirstMismatchTime.IsSome() {
		expectedLen += 1 + len("firstMismatchTime") + 1 + 8
	}

	doc := make(bson.Raw, 4, expectedLen)
	doc = bsoncore.AppendDocumentElement(doc, "_id", keyRaw)
	doc = bsoncore.AppendInt32Element(doc, "dataSize", int32(rd.DataSize))

	if cot, has := rd.ChangeOpTime.Get(); has {
		doc = bsoncore.AppendTimestampElement(doc, "changeOpTime", cot.T, cot.I)
	}

	if rd.FromDst {
		doc = bsoncore.AppendBooleanElement(doc, "fromDst", rd.FromDst)
	}

	if fmt, has := rd.FirstMismatchTime.Get(); has {
		doc = bsoncore.AppendDateTimeElement(doc, "firstMismatchTime", int64(fmt))
	}

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
		case "changeOpTime":
			var ts bson.Timestamp
			if err := mbson.UnmarshalElementValue(el, &ts); err != nil {
				return err
			}

			rd.ChangeOpTime = option.Some(ts)
		case "fromDst":
			if err := mbson.UnmarshalElementValue(el, &rd.FromDst); err != nil {
				return err
			}
		case "firstMismatchTime":
			var fmt bson.DateTime
			if err := mbson.UnmarshalElementValue(el, &fmt); err != nil {
				return err
			}

			rd.FirstMismatchTime = option.Some(fmt)
		default:
			return fmt.Errorf("unrecognized BSON field name for Go %T: %#q", *rd, key)
		}
	}

	return nil
}
