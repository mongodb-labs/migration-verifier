package mslices

import (
	"fmt"
	"reflect"
	"slices"
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

// Reorder accepts a slice and a slice of indices then
// reorders the slice in-place. For example, given a
// []string{"a", "b", "c"} and []int{1, 2, 0}, the slice
// will become {"b", "c", "a"}.
func Reorder[T any](data []T, order []int) {
	if len(order) != len(data) {
		panic(fmt.Sprintf(
			"cannot reorder: slice len=%d; indices len=%d",
			len(data),
			len(order),
		))
	}

	visited := make([]bool, len(order))

	for i := range len(order) {
		if visited[i] || order[i] == i {
			continue
		}

		current := i
		temp := data[i]

		for {
			next := order[current]
			if visited[next] {
				break
			}

			data[current] = data[next]
			visited[current] = true
			current = next
		}

		data[current] = temp
		visited[current] = true
	}
}
