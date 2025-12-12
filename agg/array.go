package agg

import (
	"slices"

	"go.mongodb.org/mongo-driver/v2/bson"
)

type Slice struct {
	Array    any
	Position *any
	N        any
}

func (s Slice) MarshalBSON() ([]byte, error) {
	args := []any{s.Array, s.N}
	if s.Position != nil {
		args = slices.Insert(args, 1, *s.Position)
	}

	return bson.Marshal(bson.D{
		{"$slice", args},
	})
}

// ---------------------------------------------

type Size [1]any

var _ bson.Marshaler = Size{}

func (s Size) MarshalBSON() ([]byte, error) {
	return bson.Marshal(bson.D{{"$size", s[0]}})
}

// ---------------------------------------------

type Map struct {
	Input, As, In any
}

var _ bson.Marshaler = Map{}

func (m Map) D() bson.D {
	return bson.D{
		{"$map", bson.D{
			{"input", m.Input},
			{"as", m.As},
			{"in", m.In},
		}},
	}
}

func (m Map) MarshalBSON() ([]byte, error) {
	return bson.Marshal(m.D())
}

// ------------------------------------------

type Filter struct {
	Input, As, Cond, Limit any
}

var _ bson.Marshaler = Filter{}

func (f Filter) D() bson.D {
	d := bson.D{
		{"input", f.Input},
		{"as", f.As},
		{"cond", f.Cond},
	}

	if f.Limit != nil {
		d = append(d, bson.E{"limit", f.Limit})
	}
	return bson.D{{"$filter", d}}
}

func (f Filter) MarshalBSON() ([]byte, error) {
	return bson.Marshal(f.D())
}

// ------------------------------------------

type Range struct {
	Start any
	End   any
	Step  any // ignored if 0
}

var _ bson.Marshaler = Range{}

func (r Range) MarshalBSON() ([]byte, error) {
	if r.Start == nil {
		r.Start = 0
	}

	args := bson.A{r.Start, r.End}

	if r.Step != nil {
		args = append(args, r.Step)
	}

	return bson.Marshal(bson.D{{"$range", args}})
}

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
