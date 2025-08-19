package mslices

import "reflect"

// This package complements the Go standard library’s package of the
// same name with broadly-useful tools that the standard library lacks.

// Of returns a slice out of the given arguments. It’s syntactic sugar
// to capitalize on Go’s type inference, similar to
// [this declined feature proposal](https://github.com/golang/go/issues/47709).
func Of[T any](pieces ...T) []T {
	return slices.Clone(pieces)
}

// ToMap outputs a map that “indexes” the given slice.
func ToMap[S ~[]E, E any, K comparable](s S, cb func(el E) K) map[K]E {
	theMap := make(map[K]E, len(s))

	for _, el := range s {
		theMap[cb(el)] = el
	}

	return theMap
}

// Compact is like samber/lo’s function of the same name, but this
// allows any type, not just comparable.
func Compact[T any, S ~[]T](slc S) S {
	var ret S

	for _, el := range slc {
		if !isZero(el) {
			ret = append(ret, el)
		}
	}

	return ret
}

func isZero[T any](val T) bool {
	return reflect.ValueOf(&val).Elem().IsZero()
}
