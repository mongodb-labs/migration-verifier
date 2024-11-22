package mslices

// This package complements the Go standard library’s package of the
// same name with broadly-useful tools that the standard library lacks.

// Of returns a slice out of the given arguments. It’s syntactic sugar
// to capitalize on Go’s type inference, similar to
// [this declined feature proposal](https://github.com/golang/go/issues/47709).
func Of[T any](pieces ...T) []T {
	return append([]T{}, pieces...)
}
