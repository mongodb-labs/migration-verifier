package util

import (
	"slices"
	"strconv"

	"github.com/10gen/migration-verifier/mbson"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
)

// BSONArraySizer is useful for constructing BSON arrays that must
// not exceed a maximum length.
type BSONArraySizer struct {
	elemsSize int
	count     int
}

// Add() adds a new element.
func (b *BSONArraySizer) Add(el bson.RawValue) {
	newElIndex := strconv.Itoa(b.count)

	// Each element contains:
	//	- a type byte
	//  - the stringified key
	//  - a NUL byte
	//  - the encoded BSON value
	//
	// We thus augment b.size accordingly:
	b.elemsSize += 1 + len(newElIndex) + 1 + len(el.Value)

	b.count++
}

// Len() returns the encoded BSON array’s length in bytes.
func (b *BSONArraySizer) Len() int {

	// 4 bytes for BSON length, the elems, then a final NUL.
	return 5 + b.elemsSize
}

// SplitArrayByBSONMaxSize takes an array of arbitrary Go values and
// groups them so that, when built into a BSON array, each group exceeds
// softMaxSize by the smallest length possible.
//
// (The max is a “soft” max because we need to accommodate the chance
// that a single item can exceed the limit.)
func SplitArrayByBSONMaxSize(ids []any, softMaxSize int) ([][]any, error) {
	ids = slices.Clone(ids)

	groups := [][]any{}

	sizer := &BSONArraySizer{}
	curGroup := []any{}
	for len(ids) > 0 {
		curGroup = append(curGroup, ids[0])

		rawVal, err := mbson.ConvertToRawValue(ids[0])
		if err != nil {
			return nil, errors.Wrapf(err, "failed to marshal %T (%v) to BSON", ids[0], ids[0])
		}
		sizer.Add(rawVal)
		ids = ids[1:]

		if sizer.Len() >= softMaxSize {
			groups = append(groups, curGroup)
			curGroup = []any{}
			sizer = &BSONArraySizer{}
		}
	}

	if len(curGroup) > 0 {
		groups = append(groups, curGroup)
	}

	return groups, nil
}
