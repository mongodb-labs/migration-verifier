package mseq

import "iter"

func FromSlice[T any](s []T) iter.Seq[T] {
	return func(yield func(T) bool) {
		for _, v := range s {
			if !yield(v) {
				return
			}
		}
	}
}

// FromSliceWithErr is like FromSlice but returns a Seq2
// whose second return is always a nil error. This is useful
// in testing.
func FromSliceWithErr[T any](s []T) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		for _, v := range s {
			if !yield(v, nil) {
				return
			}
		}
	}
}
