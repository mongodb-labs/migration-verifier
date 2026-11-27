// Package expr provides convenience types and functions for building
// aggregation expressions. This lets you write out expressions using
// syntax like:
//
//	agg.Cond{
//	    If: "$someFlag",
//	    Then: "yes",
//	    Else: "no",
//	}
//
// … rather than using [bson.D] or [bson.M]. Advantages include:
//   - protection against misspellings
//   - simpler syntax
//   - auto-completion (i.e., via gopls)
//
// See this package’s README.md for more details.
package expr

import "go.mongodb.org/mongo-driver/v2/bson"

// BSONSize is the $bsonSize operator.
type BSONSize [1]any

var _ bson.Marshaler = BSONSize{}

func (b BSONSize) D() bson.D {
	return bson.D{{"$bsonSize", b[0]}}
}

func (b BSONSize) MarshalBSON() ([]byte, error) {
	return bson.Marshal(b.D())
}

// ---------------------------------------------

// Type is the $type operator. Use [BSONType] to refer to the BSON
// types it can match.
//
// NB: helpers.TypeIs() is often more convenient.
type Type [1]any

var _ bson.Marshaler = Type{}

func (t Type) D() bson.D {
	return bson.D{{"$type", t[0]}}
}

func (t Type) MarshalBSON() ([]byte, error) {
	return bson.Marshal(t.D())
}

// ---------------------------------------------

// Not is the $not operator.
type Not [1]any

var _ bson.Marshaler = Not{}

func (n Not) D() bson.D {
	return bson.D{{"$not", n[0]}}
}

func (n Not) MarshalBSON() ([]byte, error) {
	return bson.Marshal(n.D())
}

// ---------------------------------------------

// And is the $and operator.
type And []any

var _ bson.Marshaler = And{}

func (a And) D() bson.D {
	return bson.D{{"$and", bson.A(a)}}
}

func (a And) MarshalBSON() ([]byte, error) {
	return bson.Marshal(a.D())
}

// ---------------------------------------------

// Or is the $or operator.
type Or []any

var _ bson.Marshaler = Or{}

func (o Or) D() bson.D {
	return bson.D{{"$or", bson.A(o)}}
}

func (o Or) MarshalBSON() ([]byte, error) {
	return bson.Marshal(o.D())
}

// ---------------------------------------------

// MergeObjects is the $mergeObjects operator.
type MergeObjects []any

var _ bson.Marshaler = MergeObjects{}

func (m MergeObjects) D() bson.D {
	return bson.D{{"$mergeObjects", bson.A(m)}}
}

func (m MergeObjects) MarshalBSON() ([]byte, error) {
	return bson.Marshal(m.D())
}

// ---------------------------------------------

// GetField is the $getField operator. Input is optional; it is omitted
// from the expression when nil.
type GetField struct {
	Input, Field any
}

var _ bson.Marshaler = GetField{}

func (gf GetField) D() bson.D {
	spec := bson.D{}
	if gf.Input != nil {
		spec = append(spec, bson.E{"input", gf.Input})
	}
	spec = append(spec, bson.E{"field", gf.Field})

	return bson.D{{"$getField", spec}}
}

func (gf GetField) MarshalBSON() ([]byte, error) {
	return bson.Marshal(gf.D())
}

// ---------------------------------------------

// UnsetField is the $unsetField operator.
type UnsetField struct {
	Field, Input any
}

var _ bson.Marshaler = UnsetField{}

func (uf UnsetField) D() bson.D {
	return bson.D{
		{"$unsetField", bson.D{
			{"field", uf.Field},
			{"input", uf.Input},
		}},
	}
}

func (uf UnsetField) MarshalBSON() ([]byte, error) {
	return bson.Marshal(uf.D())
}

// ---------------------------------------------

// Let is the $let operator.
type Let struct {
	Vars, In any
}

var _ bson.Marshaler = Let{}

func (l Let) D() bson.D {
	return bson.D{
		{"$let", bson.D{
			{"vars", l.Vars},
			{"in", l.In},
		}},
	}
}

func (l Let) MarshalBSON() ([]byte, error) {
	return bson.Marshal(l.D())
}

// ---------------------------------------------

// Literal is the $literal operator.
type Literal [1]any

var _ bson.Marshaler = Literal{}

func (l Literal) D() bson.D {
	return bson.D{{"$literal", l[0]}}
}

func (l Literal) MarshalBSON() ([]byte, error) {
	return bson.Marshal(l.D())
}
