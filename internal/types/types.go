package types

import (
	"golang.org/x/exp/constraints"
)

// DocumentCount represents a count of BSON/MongoDB documents.
type DocumentCount uint64

// NamespaceCount represents a count of MongoDB namespaces.
type NamespaceCount uint64

// ByteCount represents a count of bytes.
type ByteCount uint64

// RealNumber represents any real (i.e., non-complex) number type.
type RealNumber interface {
	constraints.Integer | constraints.Float
}

// ToNumericTypeOf returns a copy of the 1st parameter converted to the
// “type of” the 2nd parameter. For example, if you’ve designated a value
// as an `int32`, and you want to compare that value to another one
// somewhere else, it’s nice not to have to duplicate the `int32`. This
// lets you do that.
//
// Example:
// ```
//
//	limit := int32(234)
//	i := ToNumericTypeOf(0, limit)
//
//	for i < limit {
//	    i++
//	    // ...
//	}
//
// ```
func ToNumericTypeOf[To, From RealNumber](value From, _ To) To {
	return To(value)
}
