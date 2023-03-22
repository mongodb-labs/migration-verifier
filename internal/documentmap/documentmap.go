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
	"context"
	"fmt"

	"github.com/10gen/migration-verifier/internal/logger"
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
func (m *Map) ImportFromCursor(ctx context.Context, cursor *mongo.Cursor) error {
	if m.imported {
		panic("Refuse duplicate call!")
	}

	m.imported = true

	var bytesReturned int64
	bytesReturned, nDocumentsReturned := 0, 0

	for cursor.Next(ctx) {
		err := cursor.Err()
		if err != nil {
			if err == mongo.ErrNoDocuments {
				break
			}

			return err
		}

		nDocumentsReturned++

		var rawDoc bson.Raw
		err = cursor.Decode(&rawDoc)
		if err != nil {
			return err
		}
		bytesReturned += (int64)(len(rawDoc))

		m.addDocument(rawDoc)
	}
	m.logger.Debug().Msgf("Find returned %d documents containing %d bytes", nDocumentsReturned, bytesReturned)

	return nil
}

func (m *Map) addDocument(rawDoc bson.Raw) {
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

// ----------------------------------------------------------------------

// called in tests as well as internally
func (m *Map) getMapKey(doc *bson.Raw) MapKey {
	key := MapKey("")
	for _, keyName := range m.fieldNames {
		value := doc.Lookup(keyName)
		key += MapKey(value.Type) + MapKey(value.Value)
	}

	return key
}
