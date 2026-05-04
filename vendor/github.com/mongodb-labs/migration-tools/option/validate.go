package option

import (
	"reflect"
	"slices"
)

var nilableKinds = []reflect.Kind{
	reflect.Chan,
	reflect.Func,
	reflect.Interface,
	reflect.Map,
	reflect.Pointer,
	reflect.Slice,
}

// This is generic to avoid boxing.
func isNil[T any](val T) bool {
	if slices.Contains(nilableKinds, reflect.TypeFor[T]().Kind()) {
		// Just because T is a nilable kind doesn’t mean that the dynamic type
		// is nilable. For example, any(4) will cause TypeFor() to
		// return interface, which is nilable, even though the dynamic type
		// (int) is not.
		rVal := reflect.ValueOf(val)

		// reflect.ValueOf(any(nil)) returns an invalid Value,
		// so we have to check for that first.
		if !rVal.IsValid() {
			return true
		}

		return slices.Contains(nilableKinds, rVal.Kind()) && rVal.IsNil()
	}

	return false
}
