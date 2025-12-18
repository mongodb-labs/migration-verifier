package verifier

import (
	"github.com/10gen/migration-verifier/option"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// The following correlates a given doc’s first-mismatch time from a task.
// This is nontrivial because the task stores first-mismatch times indexed
// on the document ID’s position in the `_ids` array.
//
// This lookup depends on unchanged document IDs. If the document’s ID changes
// numeric type, for example, this won’t return a result. That’s OK, though,
// because the point of mismatch counting is to track the number of times a
// document was compared *without* a change, and a change of numeric type means
// there was a change event, which invalidates the document’s stored
// first-mismatch time.
type firstMismatchTimeLookup struct {
	task             *VerificationTask
	docCompareMethod DocCompareMethod

	// a cache:
	idToFirstMismatchTime map[string]bson.DateTime
}

func (fl *firstMismatchTimeLookup) get(doc bson.Raw) option.Option[bson.DateTime] {
	if fl.idToFirstMismatchTime == nil {
		fl.idToFirstMismatchTime = createIDToFirstMismatchTime(fl.task)
	}

	mapKeyBytes := rvToMapKey(
		nil,
		getDocIdFromComparison(fl.docCompareMethod, doc),
	)

	return option.IfNotZero(fl.idToFirstMismatchTime[string(mapKeyBytes)])
}

func createIDToFirstMismatchTime(task *VerificationTask) map[string]bson.DateTime {
	idToFirstMismatchTime := map[string]bson.DateTime{}

	for i, id := range task.Ids {
		firstTime := task.FirstMismatchTime[int32(i)]

		if firstTime != 0 {
			idToFirstMismatchTime[string(rvToMapKey(nil, id))] = firstTime
		}
	}

	return idToFirstMismatchTime
}
