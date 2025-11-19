// Package agg provides convenience types for aggregation operators.
// This yields two major advantages over using bson.D or bson.M:
// - simpler syntax
// - auto-completion (i.e., via gopls)
//
// Guiding principles are:
// - Prefer [1]any for unary operators (e.g., `$bsonSize`).
// - Prefer struct types for operators with named parameters.
// - Use functions sparingly, e.g., for “tuple” operators like `$in`.
package agg

import (
	"go.mongodb.org/mongo-driver/v2/bson"
)

type Eq []any

var _ bson.Marshaler = Eq{}

func (e Eq) MarshalBSON() ([]byte, error) {
	return bson.Marshal(bson.D{{"$eq", []any(e)}})
}

// ---------------------------------------------

func In[T any](needle any, haystack []T) bson.D {
	return bson.D{{"$in", bson.A{needle, haystack}}}
}

// ---------------------------------------------

type BSONSize [1]any

var _ bson.Marshaler = BSONSize{}

func (b BSONSize) MarshalBSON() ([]byte, error) {
	return bson.Marshal(bson.D{{"$bsonSize", b[0]}})
}

// ---------------------------------------------

type Type [1]any

var _ bson.Marshaler = Type{}

func (t Type) MarshalBSON() ([]byte, error) {
	return bson.Marshal(bson.D{{"$type", t[0]}})
}

// ---------------------------------------------

type Not [1]any

var _ bson.Marshaler = Type{}

func (n Not) MarshalBSON() ([]byte, error) {
	return bson.Marshal(bson.D{{"$not", n[0]}})
}

// ---------------------------------------------

type And []any

var _ bson.Marshaler = And{}

func (a And) MarshalBSON() ([]byte, error) {
	return bson.Marshal(bson.D{
		{"$and", []any(a)},
	})
}

// ---------------------------------------------

type Or []any

var _ bson.Marshaler = Or{}

func (o Or) MarshalBSON() ([]byte, error) {
	return bson.Marshal(bson.D{
		{"$or", []any(o)},
	})
}

// ---------------------------------------------

type MergeObjects []any

var _ bson.Marshaler = MergeObjects{}

func (m MergeObjects) MarshalBSON() ([]byte, error) {
	return bson.Marshal(bson.D{
		{"$mergeObjects", []any(m)},
	})
}

// ---------------------------------------------

type Cond struct {
	If, Then, Else any
}

var _ bson.Marshaler = Cond{}

func (c Cond) D() bson.D {
	return bson.D{
		{"$cond", bson.D{
			{"if", c.If},
			{"then", c.Then},
			{"else", c.Else},
		}},
	}
}

func (c Cond) MarshalBSON() ([]byte, error) {
	return bson.Marshal(c.D())
}

// ---------------------------------------------

type Switch struct {
	Branches []SwitchCase
	Default  any
}

var _ bson.Marshaler = Switch{}

type SwitchCase struct {
	Case any
	Then any
}

func (s Switch) D() bson.D {
	return bson.D{{"$switch", bson.D{
		{"branches", s.Branches},
		{"default", s.Default},
	}}}
}

func (s Switch) MarshalBSON() ([]byte, error) {
	return bson.Marshal(s.D())
}

// ---------------------------------------------

type ArrayElemAt struct {
	Array any
	Index int
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
