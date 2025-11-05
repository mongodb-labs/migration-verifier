package recheck

import (
	"encoding/binary"
	"fmt"

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

var _ bson.Marshaler = &PrimaryKey{}

// MarshalBSON implements bson.Marshaler .. which is only done to prevent
// the inefficiency of bson.Marshal().
func (rk PrimaryKey) MarshalBSON() ([]byte, error) {
	panic("Use MarshalToBSON instead.")
}

func (rk PrimaryKey) MarshalToBSON() ([]byte, error) {
	// This is a very “hot” path, so we want to minimize allocations.
	variableSize := len(rk.SrcDatabaseName) + len(rk.SrcCollectionName) + len(rk.DocumentID.Value)

	// This document’s nonvariable parts comprise 32 bytes.
	expectedLen := 32 + variableSize

	doc := make(bson.Raw, 4, expectedLen)

	doc = bsoncore.AppendStringElement(doc, "db", rk.SrcDatabaseName)
	doc = bsoncore.AppendStringElement(doc, "coll", rk.SrcCollectionName)
	doc = bsoncore.AppendValueElement(doc, "docID", bsoncore.Value{
		Type: bsoncore.Type(rk.DocumentID.Type),
		Data: rk.DocumentID.Value,
	})

	doc = append(doc, 0)

	if len(doc) != expectedLen {
		panic(fmt.Sprintf("Unexpected %T BSON size %d; expected %d", rk, len(doc), expectedLen))
	}

	binary.LittleEndian.PutUint32(doc, uint32(len(doc)))

	return doc, nil
}

// Doc stores the necessary information to know which documents must be rechecked.
type Doc struct {
	PrimaryKey PrimaryKey `bson:"_id"`

	// NB: Because we don’t update the recheck queue’s documents, this field
	// and any others that may be added will remain unchanged even if a recheck
	// is enqueued multiple times for the same document in the same generation.
	DataSize int32 `bson:"dataSize"`
}

var _ bson.Marshaler = &Doc{}

// MarshalBSON implements bson.Marshaler .. which is only done to prevent
// the inefficiency of bson.Marshal().
func (rd Doc) MarshalBSON() ([]byte, error) {
	panic("Use MarshalToBSON instead.")
}

func (rd Doc) MarshalToBSON() ([]byte, error) {
	keyRaw, err := rd.PrimaryKey.MarshalToBSON()
	if err != nil {
		return nil, errors.Wrapf(err, "marshaling recheck primary key")
	}

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

	return doc, nil
}
