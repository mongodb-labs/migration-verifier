// Package option implements [option types] in Go.
// It takes inspiration from [samber/mo] but also works with BSON and exposes
// a (hopefully) more refined interface.
//
// Typical Go code uses pointers to represent such values. That’s problematic
// for 2 reasons:
// - If you accidentally dereference a nil pointer, your program panics.
// - The pointer can increase GC pressure.
//
// This type solves both of those.
//
// A couple special notes:
//   - nil values inside the Option, like `Some([]int(nil))`, are forbidden.
//     (A runtime panic will happen if you try.) See functions like
//     FromPointer and IfNotZero instead.
//   - Option’s BSON marshaling/unmarshaling interoperates with the [bson]
//     package’s handling of nilable pointers. So any code that uses nilable
//     pointers to represent optional values can switch to Option and
//     should continue working with existing persisted data.
//   - Because encoding/json provides no equivalent to bsoncodec.Zeroer,
//     Option always marshals to JSON null if empty.
//
// Prefer Option to nilable pointers in all new code, and consider
// changing existing code to use it.
//
// [option types]: https://en.wikipedia.org/wiki/Option_type
package option

import (
	"reflect"

	"github.com/samber/lo"
)

// Option represents a possibly-empty value.
// Its zero value is the empty case.
type Option[T any] struct {
	isSet bool
	val   T
}

// Some creates an Option with a value.
func Some[T any](value T) Option[T] {
	lo.Assertf(
		!isNil(value),
		"Option[%T] forbids nil value",
		value,
	)

	return Option[T]{true, value}
}

// None creates an Option with no value.
//
// Note that `None[T]()` is interchangeable with `Option[T]{}`.
func None[T any]() Option[T] {
	return Option[T]{}
}

// FromPointer will convert a nilable pointer into its
// equivalent Option.
func FromPointer[T any](valPtr *T) Option[T] {
	if valPtr == nil {
		return None[T]()
	}

	lo.Assertf(
		!isNil(*valPtr),
		"Given %T refers to nil, which is forbidden.",
		valPtr,
	)

	return Option[T]{true, *valPtr}
}

// IfNotZero returns an Option that’s populated if & only if
// the given value is a non-zero value. (NB: The zero value
// for slices & maps is nil, not empty!)
//
// This is useful, e.g., to interface with code that uses
// nil to indicate a missing slice or map.
func IfNotZero[T any](value T) Option[T] {

	// copied from samber/mo.EmptyableToOption:
	if reflect.ValueOf(&value).Elem().IsZero() {
		return Option[T]{}
	}

	return Option[T]{true, value}
}

// Get “unboxes” the Option’s internal value.
// The boolean indicates whether the value exists.
func (o Option[T]) Get() (T, bool) {
	if !o.isSet {
		return *new(T), false
	}

	return o.val, true
}

// MustGet is like Get but panics if the Option is empty.
func (o Option[T]) MustGet() T {
	return o.MustGetf("%T must be nonempty!", o)
}

// MustGetf is like MustGet, but this lets you customize the panic.
func (o Option[T]) MustGetf(pattern string, args ...any) T {
	val, exists := o.Get()
	lo.Assertf(exists, pattern, args...)

	return val
}

// OrZero returns either the Option’s internal value or
// the type’s zero value.
func (o Option[T]) OrZero() T {
	val, exists := o.Get()
	if exists {
		return val
	}

	return *new(T)
}

// OrElse returns either the Option’s internal value or
// the given `fallback`.
func (o Option[T]) OrElse(fallback T) T {
	val, exists := o.Get()
	if exists {
		return val
	}

	return fallback
}

// ToPointer converts the Option to a nilable pointer.
// The internal value (if it exists) is (shallow-)copied.
func (o Option[T]) ToPointer() *T {
	val, exists := o.Get()
	if exists {
		theCopy := val
		return &theCopy
	}

	return nil
}

// IsNone returns a boolean indicating whether or not the option is a None
// value.
func (o Option[T]) IsNone() bool {
	return !o.isSet
}

// IsSome returns a boolean indicating whether or not the option is a Some
// value.
func (o Option[T]) IsSome() bool {
	return o.isSet
}
