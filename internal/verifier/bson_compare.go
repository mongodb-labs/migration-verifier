package verifier

import (
	"fmt"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsontype"
)

type MismatchDetails struct {
	missingFieldOnSrc   []string
	missingFieldOnDst   []string
	fieldContentsDiffer []string
}

// Compares two bson documents, ignoring order in the document and subdocuments, and returns the details
// for the top level field only.  Returns nil if the documents match
// stopOnMismatch: true if comparison should stop with the first mismatched field
func BsonUnorderedCompareRawDocumentWithDetails(srcRaw, dstRaw bson.Raw) (*MismatchDetails, error) {
	srcElements, dstElements, err := parseDocuments(srcRaw, dstRaw)
	if err != nil {
		return nil, err
	}
	return bsonUnorderedCompareRawElements(srcElements, dstElements, false /* stopOnMismatch */)
}

// Compares two bson documents, returns true if they match.  No details provided.
func BsonUnorderedCompareRawDocument(srcRaw, dstRaw bson.Raw) (bool, error) {
	srcElements, dstElements, err := parseDocuments(srcRaw, dstRaw)
	if err != nil {
		return false, err
	}
	result, err := bsonUnorderedCompareRawElements(srcElements, dstElements, true /* stopOnMismatch */)
	return result == nil, err
}

func parseDocuments(srcRaw, dstRaw bson.Raw) (srcElements, dstElements []bson.RawElement, err error) {
	srcElements, err = srcRaw.Elements()
	if err != nil {
		err = fmt.Errorf("Error parsing source document for compare: %s", err)
		return
	}
	dstElements, err = dstRaw.Elements()
	if err != nil {
		err = fmt.Errorf("Error parsing dest document for compare: %s", err)
		srcElements = nil
		return
	}
	return
}

// Compares two sets of bson elements, ignoring order.
// Returns all mismatches if stopOnMismatch is false, only the first if it is true.
func bsonUnorderedCompareRawElements(srcElements, dstElements []bson.RawElement, stopOnMismatch bool) (*MismatchDetails, error) {
	var mismatchDetails MismatchDetails
	anyMismatch := false
	srcMap := map[string]bson.RawValue{}
	srcMapUsed := map[string]bool{}
	for _, v := range srcElements {
		key := v.Key()
		srcMap[key] = v.Value()
	}

	for _, dstElement := range dstElements {
		key, dstValue := dstElement.Key(), dstElement.Value()
		srcValue, ok := srcMap[key]
		if !ok {
			mismatchDetails.missingFieldOnSrc = append(mismatchDetails.missingFieldOnSrc, key)
			anyMismatch = true
		} else {
			srcMapUsed[key] = true
			result, err := bsonUnorderedCompareRawValue(srcValue, dstValue)
			if err != nil {
				return nil, err
			}
			if !result {
				mismatchDetails.fieldContentsDiffer = append(mismatchDetails.fieldContentsDiffer, key)
				anyMismatch = true
			}
		}
		if stopOnMismatch && anyMismatch {
			return &mismatchDetails, nil
		}
	}

	// If we haven't checked all the source elements, there must be a source key not used (missing from dest)
	if len(srcMap) != len(srcMapUsed) {
		for key := range srcMap {
			_, ok := srcMapUsed[key]
			if !ok {
				mismatchDetails.missingFieldOnDst = append(mismatchDetails.missingFieldOnDst, key)
				anyMismatch = true
				if stopOnMismatch {
					return &mismatchDetails, nil
				}
			}
		}
	}
	if anyMismatch {
		return &mismatchDetails, nil
	}
	return nil, nil
}

// Returns true if the values match, ignoring order in any subdocuments.
func bsonUnorderedCompareRawValue(srcValue, dstValue bson.RawValue) (bool, error) {
	if srcValue.Type != dstValue.Type {
		return false, nil
	}

	switch srcValue.Type {
	case bsontype.Array:
		return bsonUnorderedCompareRawArray(srcValue.Array(), dstValue.Array())
	case bsontype.EmbeddedDocument:
		return BsonUnorderedCompareRawDocument(srcValue.Document(), dstValue.Document())
	default:
		return srcValue.Equal(dstValue), nil
	}
}

// Compares two bson arrays, comparing subdocuments ignoring order.  The array order is still significant.
// Returns true if the arrays match.
func bsonUnorderedCompareRawArray(srcRaw, dstRaw bson.Raw) (bool, error) {

	srcElements, dstElements, err := parseDocuments(srcRaw, dstRaw)
	if err != nil {
		return false, err
	}
	if len(srcElements) != len(dstElements) {
		return false, nil
	}
	for i, srcElement := range srcElements {
		dstElement := dstElements[i]
		if srcElement.Key() != dstElement.Key() {
			return false, fmt.Errorf("Array keys differ: %s %s", srcElement.Key(), dstElement.Key())
		}
		matches, err := bsonUnorderedCompareRawValue(srcElement.Value(), dstElement.Value())
		if err != nil || !matches {
			return false, err
		}
	}
	return true, nil
}
