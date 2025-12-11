// Package agg provides convenience types for aggregation operators.
// This yields two major advantages over using bson.D or bson.M:
// - simpler syntax
// - auto-completion (i.e., via gopls)
//
// Guiding principles are:
//   - Prefer [1]any for 1-arg operators (e.g., `$bsonSize`).
//   - Prefer [2]any for binary operators whose arguments don’t benefit
//     from naming. (e.g., $eq)
//   - Prefer struct types for operators with named parameters.
//   - Prefer struct types for operators whose documentation gives names,
//     even if those names aren’t sent to the server.
//   - Use functions sparingly, e.g., for “tuple” operators like `$in`.
package agg

import (
	"go.mongodb.org/mongo-driver/v2/bson"
)

type Eq [2]any

var _ bson.Marshaler = Eq{}

func (e Eq) MarshalBSON() ([]byte, error) {
	return bson.Marshal(bson.D{{"$eq", [2]any(e)}})
}

// ---------------------------------------------

type Gt [2]any

func (g Gt) MarshalBSON() ([]byte, error) {
	return bson.Marshal(bson.D{{"$gt", [2]any(g)}})
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

var _ bson.Marshaler = Not{}

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

type GetField struct {
	Input, Field any
}

var _ bson.Marshaler = GetField{}

func (gf GetField) MarshalBSON() ([]byte, error) {
	return bson.Marshal(
		bson.D{
			{"$getField", bson.D{
				{"input", gf.Input},
				{"field", gf.Field},
			}},
		},
	)
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
