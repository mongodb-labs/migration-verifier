package util

import (
	"strconv"

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
