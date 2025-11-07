package mcmp

import "reflect"

// Or is like the standard library’s cmp.Or(), but it accepts any type.
// It’s slower underneath, though.
func Or[T any](vals ...T) T {
	for _, val := range vals {
		if !reflect.ValueOf(&val).Elem().IsZero() {
			return val
		}
	}

	return *new(T)
}
