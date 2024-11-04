package documentmap

// # Overview
//
// This package defines logic to match up two sets of documents.
//
// Originally this was a simple map on `_id` values, but that doesn’t
// work in sharded clusters with duplicate `_id` values within a
// collection. (As of this writing mongosync doesn’t actually support
// duplicate-ID scenarios, but other such tools, like mongomirror, do.)
//
// The present implementation, then, incorporates the values of `_id`
// as well as the shard-key values. So, for example, if a collection
// shards on field `foo`, and the collection contains these documents
// on different shards:
//
//    { _id: 1, foo: "abc", bar: 123 }
//    { _id: 1, foo: "xyz", bar: 234 }
//
// … then the verifier can use this package to compare those documents
// reliably on the source & destination cluster.
//
// # Caveats
//
// This method assumes that a given value is stored in both source &
// destination as the same type. (The MongoDB server has logic for
// comparing mismatched-type values, but it would seem unreasonable
// to try to duplicate that here.)

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/memorytracker"
	"github.com/10gen/migration-verifier/internal/reportutils"
	"github.com/10gen/migration-verifier/internal/types"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

const (
	idFieldName = "_id"
)

// MapKey represents how this package internally indexes documents.
// Consumers should not know or care what the type’s actual values are.
//
//go:hiddendoc Ideally we’d use []byte rather than string, but we need
//go:hiddendoc a type that satisfies the `comparable` constraint.
type MapKey string

// This is what actually indexes documents for the Map struct.
type mapKeyToDocMap map[MapKey]bson.Raw

// Map is the main struct for this package.
type Map struct {
	internalMap mapKeyToDocMap
	logger      *logger.Logger

	// This always includes idFieldName
	fieldNames []string
	imported   bool
}

// New creates a Map. Each `names` is the name of a field to
// incorporate into the indexing. (Note: `_id` is always included,
// so don’t give it explicitly.)
func New(logger *logger.Logger, shardFieldNames ...string) *Map {
	newMap := Map{
		logger:      logger,
		fieldNames:  []string{idFieldName},
		internalMap: mapKeyToDocMap{},
	}

	newMap.fieldNames = append(newMap.fieldNames, shardFieldNames...)

	return &newMap
}

// CloneEmpty clones a Map, discarding any “accrued” state.
func (m *Map) CloneEmpty() *Map {
	myCopy := *m
	myCopy.imported = false
	myCopy.internalMap = mapKeyToDocMap{}

	return &myCopy
}

// ImportFromCursor populates the Map from the given cursor.
// This can take a while, so it should probably happen in its
// own goroutine.
//
// As a safeguard, this panics if called more than once.
func (m *Map) ImportFromCursor(ctx context.Context, cursor *mongo.Cursor, trackerWriter memorytracker.Writer) error {
	if m.imported {
		panic("Refuse duplicate call!")
	}

	m.imported = true

	var bytesReturned uint64
	bytesReturned, nDocumentsReturned := 0, 0

	for cursor.Next(ctx) {
		err := cursor.Err()
		if err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				break
			}

			return err
		}

		nDocumentsReturned++
		bytesReturned += (uint64)(len(cursor.Current))

		// This will block if needs be to prevent OOMs.
		trackerWriter <- memorytracker.Unit(bytesReturned)

		m.copyAndAddDocument(cursor.Current)
	}
	m.logger.Debug().
		Int("documentedReturned", nDocumentsReturned).
		Str("totalSize", reportutils.BytesToUnit(bytesReturned, reportutils.FindBestUnit(bytesReturned))).
		Msgf("Finished reading %#q query.", "find")

	return nil
}

func (m *Map) copyAndAddDocument(rawDoc bson.Raw) {
	rawDocCopy := make(bson.Raw, len(rawDoc))
	copy(rawDocCopy, rawDoc)

	m.internalMap[m.getMapKey(&rawDocCopy)] = rawDocCopy
}

// CompareToMap compares the receiver with another Map.
// This method returns three arrays:
//
// 1. MapKeys that exist solely in the receiver Map.
//
// 2. MapKeys that exist solely in `other`.
//
// 3. MapKeys that exist in both.
func (m *Map) CompareToMap(other *Map) ([]MapKey, []MapKey, []MapKey) {
	meOnly := []MapKey{}
	otherOnly := []MapKey{}
	common := []MapKey{}

	// Worthwhile to parallelize?

	for key := range m.internalMap {
		_, exists := other.internalMap[key]
		if exists {
			common = append(common, key)
		} else {
			meOnly = append(meOnly, key)
		}
	}

	for key := range other.internalMap {
		_, exists := m.internalMap[key]

		if !exists {
			otherOnly = append(otherOnly, key)
		}
	}

	return meOnly, otherOnly, common
}

// Fetch fetches a document from the Map given its MapKey.
//
// Since we really don’t expect the MapKey not to exist,
// this panics if no such document is found.
func (m *Map) Fetch(key MapKey) bson.Raw {
	doc, exists := m.internalMap[key]
	if !exists {
		panic(fmt.Sprintf("Map lacks document with key %v", key))
	}

	return doc
}

// Count returns the number of documents in the Map.
func (m *Map) Count() types.DocumentCount {
	return types.DocumentCount(len(m.internalMap))
}

// TotalDocsBytes returns the combined byte size of the Map’s documents.
func (m *Map) TotalDocsBytes() types.ByteCount {
	var size types.ByteCount
	for _, doc := range m.internalMap {
		size += types.ByteCount(len(doc))
	}

	return size
}

// ----------------------------------------------------------------------

// called in tests as well as internally
func (m *Map) getMapKey(doc *bson.Raw) MapKey {
	var keyBuffer bytes.Buffer
	for _, keyName := range m.fieldNames {
		value := doc.Lookup(keyName)
		keyBuffer.Grow(1 + len(value.Value))
		keyBuffer.WriteByte(byte(value.Type))
		keyBuffer.Write(value.Value)
	}

	return MapKey(keyBuffer.String())
}
