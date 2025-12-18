package verifier

import (
	"github.com/10gen/migration-verifier/option"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// The following retrieves a given document from a recheck task with metadata
// for that document. This is nontrivial because the task stores such metadata
// indexed on the document ID’s position in the `_ids` array.
//
// These lookups depend on unchanged document IDs. If the document’s ID changes
// numeric type, for example, this won’t return a result. That’s OK, though,
// because:
//
//   - For mismatches: the point here is to track the number of times a document
//     was compared *without* a change, and a change of ID means there was a
//     change event, which invalidates the stored first-mismatch time.
//   - For change optimes: This is “less OK” than for mismatches, but it’s not a
//     concern because there’ll be another change event soon enough that has the
//     new document ID.
type docMetadataLookup struct {
	task             *VerificationTask
	docCompareMethod DocCompareMethod

	// a cache:
	idToFirstMismatchTime map[string]bson.DateTime
	idToSrcChangeOptime   map[string]bson.Timestamp
	idToDstChangeOptime   map[string]bson.Timestamp
}

func (fl *docMetadataLookup) getFirstMismatchTime(doc bson.Raw) option.Option[bson.DateTime] {
	if fl.idToFirstMismatchTime == nil {
		fl.createCaches()
	}

	mapKeyBytes := rvToMapKey(
		nil,
		getDocIdFromComparison(fl.docCompareMethod, doc),
	)

	return option.IfNotZero(fl.idToFirstMismatchTime[string(mapKeyBytes)])
}

func (fl *docMetadataLookup) getSrcChangeOpTime(doc bson.Raw) option.Option[bson.Timestamp] {
	if fl.idToFirstMismatchTime == nil {
		fl.createCaches()
	}

	mapKeyBytes := rvToMapKey(
		nil,
		getDocIdFromComparison(fl.docCompareMethod, doc),
	)

	return option.IfNotZero(fl.idToSrcChangeOptime[string(mapKeyBytes)])
}

func (fl *docMetadataLookup) getDstChangeOpTime(doc bson.Raw) option.Option[bson.Timestamp] {
	if fl.idToFirstMismatchTime == nil {
		fl.createCaches()
	}

	mapKeyBytes := rvToMapKey(
		nil,
		getDocIdFromComparison(fl.docCompareMethod, doc),
	)

	return option.IfNotZero(fl.idToDstChangeOptime[string(mapKeyBytes)])
}

func (fl *docMetadataLookup) createCaches() {
	idToFirstMismatchTime := map[string]bson.DateTime{}
	idToSrcChangeOptime := map[string]bson.Timestamp{}
	idToDstChangeOptime := map[string]bson.Timestamp{}

	for i, id := range fl.task.Ids {
		var mapKey string

		if firstTime, ok := fl.task.FirstMismatchTime[int32(i)]; ok {
			if mapKey == "" {
				mapKey = string(rvToMapKey(nil, id))
			}

			idToFirstMismatchTime[mapKey] = firstTime
		}

		if ot, ok := fl.task.SrcChangeOpTime[int32(i)]; ok {
			if mapKey == "" {
				mapKey = string(rvToMapKey(nil, id))
			}

			idToSrcChangeOptime[mapKey] = ot
		}

		if ot, ok := fl.task.DstChangeOpTime[int32(i)]; ok {
			if mapKey == "" {
				mapKey = string(rvToMapKey(nil, id))
			}

			idToDstChangeOptime[mapKey] = ot
		}
	}

	fl.idToFirstMismatchTime = idToFirstMismatchTime
	fl.idToSrcChangeOptime = idToSrcChangeOptime
	fl.idToDstChangeOptime = idToDstChangeOptime
}
