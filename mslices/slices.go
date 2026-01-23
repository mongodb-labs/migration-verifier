package mslices

import (
	"iter"
	"reflect"
	"slices"

	"github.com/10gen/migration-verifier/option"
	"github.com/samber/lo"
)

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

// FindFirstDupe returns the first item in the slice that has at
// least 1 duplicate, or none if all slice members are unique.
func FindFirstDupe[T comparable](items []T) option.Option[T] {
	for i := range items {
		j := 1 + i

		if j == len(items) {
			break
		}

		for ; j < len(items); j++ {
			if items[i] == items[j] {
				return option.Some(items[i])
			}
		}
	}

	return option.None[T]()
}

// Map1 is like lo.Map, but the callback accepts only a single parameter.
// This facilitates a lot of syntactic niceties that lo.Map makes difficult.
// For example, you can stringify a slice of `fmt.Stringer`s thus:
//
//	strings := Map1( items, theType.String )
//
// … which, with lo.Map, requires a wrapper callback.
func Map1[T any, V any](s []T, cb func(T) V) []V {
	return lo.Map(
		s,
		func(d T, _ int) V {
			return cb(d)
		},
	)
}

// Chunk is like lo.Chunk but returns an iterator.
//
// Note that this does NO CLONING. So if you’re reusing the given slice,
// be sure to clone it beforehand.
func Chunk[T any, Slice ~[]T](collection Slice, size int) iter.Seq[Slice] {
	return func(yield func(Slice) bool) {
		i := 0

		for i < len(collection) {
			chunk := lo.Slice(collection, i, i+size)

			if !yield(chunk) {
				return
			}

			i += len(chunk)
		}
	}
}
