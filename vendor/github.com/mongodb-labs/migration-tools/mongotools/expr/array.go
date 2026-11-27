package expr

import (
	"slices"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Slice is the $slice operator. Position is optional; since it is
// positional in $slice’s argument list, it is only sent when non-nil.
type Slice struct {
	Array    any
	Position *any
	N        any
}

var _ bson.Marshaler = Slice{}

func (s Slice) D() bson.D {
	args := bson.A{s.Array, s.N}
	if s.Position != nil {
		args = slices.Insert(args, 1, *s.Position)
	}

	return bson.D{{"$slice", args}}
}

func (s Slice) MarshalBSON() ([]byte, error) {
	return bson.Marshal(s.D())
}

// ----------------------------

// ArrayElemAt is the $arrayElemAt operator.
type ArrayElemAt struct {
	Array any
	Index any
}

var _ bson.Marshaler = ArrayElemAt{}

func (a ArrayElemAt) D() bson.D {
	return bson.D{{"$arrayElemAt", bson.A{
		a.Array,
		a.Index,
	}}}
}

func (a ArrayElemAt) MarshalBSON() ([]byte, error) {
	return bson.Marshal(a.D())
}

// ----------------------------

// Size is the $size operator.
type Size [1]any

var _ bson.Marshaler = Size{}

func (s Size) D() bson.D {
	return bson.D{{"$size", s[0]}}
}

func (s Size) MarshalBSON() ([]byte, error) {
	return bson.Marshal(s.D())
}

// ----------------------------

// SetDifference is the $setDifference operator.
type SetDifference [2]any

var _ bson.Marshaler = SetDifference{}

func (s SetDifference) D() bson.D {
	return bson.D{{"$setDifference", bson.A(s[:])}}
}

func (s SetDifference) MarshalBSON() ([]byte, error) {
	return bson.Marshal(s.D())
}

// ----------------------------

// SetIntersection is the $setIntersection operator.
type SetIntersection []any

var _ bson.Marshaler = SetIntersection{}

func (s SetIntersection) D() bson.D {
	return bson.D{{"$setIntersection", bson.A(s)}}
}

func (s SetIntersection) MarshalBSON() ([]byte, error) {
	return bson.Marshal(s.D())
}

// ----------------------------

// SetUnion is the $setUnion operator.
//
// See accum.SetUnion for the related accumulation operator.
type SetUnion []any

var _ bson.Marshaler = SetUnion{}

func (s SetUnion) D() bson.D {
	return bson.D{{"$setUnion", bson.A(s)}}
}

func (s SetUnion) MarshalBSON() ([]byte, error) {
	return bson.Marshal(s.D())
}

// ---------------------------------------------

// Reduce is the $reduce operator.
type Reduce struct {
	Input, InitialValue, In any
}

var _ bson.Marshaler = Reduce{}

func (r Reduce) D() bson.D {
	return bson.D{
		{"$reduce", bson.D{
			{"input", r.Input},
			{"initialValue", r.InitialValue},
			{"in", r.In},
		}},
	}
}

func (r Reduce) MarshalBSON() ([]byte, error) {
	return bson.Marshal(r.D())
}

// ---------------------------------------------

// Map is the $map operator. As is optional; it is omitted from the
// expression when nil.
type Map struct {
	Input, As, In any
}

var _ bson.Marshaler = Map{}

func (m Map) D() bson.D {
	spec := bson.D{
		{"input", m.Input},
		{"in", m.In},
	}
	if m.As != nil {
		spec = slices.Insert(spec, 1, bson.E{"as", m.As})
	}

	return bson.D{{"$map", spec}}
}

func (m Map) MarshalBSON() ([]byte, error) {
	return bson.Marshal(m.D())
}

// ---------------------------------------------

// Filter is the $filter operator. As and Limit are optional; they are
// omitted from the expression when nil.
type Filter struct {
	Input, As, Cond, Limit any
}

var _ bson.Marshaler = Filter{}

func (f Filter) D() bson.D {
	d := bson.D{
		{"input", f.Input},
		{"cond", f.Cond},
	}

	if f.As != nil {
		d = append(d, bson.E{"as", f.As})
	}

	if f.Limit != nil {
		d = append(d, bson.E{"limit", f.Limit})
	}
	return bson.D{{"$filter", d}}
}

func (f Filter) MarshalBSON() ([]byte, error) {
	return bson.Marshal(f.D())
}
