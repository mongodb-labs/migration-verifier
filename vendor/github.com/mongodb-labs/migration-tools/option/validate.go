package option

import (
	"reflect"
	"slices"
)

var nilable = []reflect.Kind{
	reflect.Chan,
	reflect.Func,
	reflect.Interface,
	reflect.Map,
	reflect.Pointer,
	reflect.Slice,
}

func isNil(val any) bool {
	if val == nil {
		return true
	}

	if slices.Contains(nilable, reflect.TypeOf(val).Kind()) {
		return reflect.ValueOf(val).IsNil()
	}

	return false
}
