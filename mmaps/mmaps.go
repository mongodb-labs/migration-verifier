package mmaps

// NewEmpty returns an empty map of the same type as
// the passed-in value.
func NewEmpty[K comparable, V any, M ~map[K]V](M) M {
	return M(map[K]V{})
}

// Init sets the referent map to a new, empty one.
func Init[K comparable, V any, M ~map[K]V](r *M) {
	*r = NewEmpty(*r)
}
