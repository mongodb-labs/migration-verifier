package mseq

import "iter"

// FromSlice returns an iterator over a slice.
//
// NB: See slices.Collect for the opposite operation.
func FromSlice[T any](s []T) iter.Seq[T] {
	return func(yield func(T) bool) {
		for _, v := range s {
			if !yield(v) {
				return
			}
		}
	}
}

// FromSliceWithNilErr is like FromSlice but returns a Seq2
// whose second return is always a nil error. This is useful
// in testing.
func FromSliceWithNilErr[T any](s []T) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		for _, v := range s {
			if !yield(v, nil) {
				return
			}
		}
	}
}
