package expr

import "go.mongodb.org/mongo-driver/v2/bson"

// Abs is the $abs operator.
type Abs [1]any

var _ bson.Marshaler = Abs{}

func (a Abs) D() bson.D {
	return bson.D{{"$abs", a[0]}}
}

func (a Abs) MarshalBSON() ([]byte, error) {
	return bson.Marshal(a.D())
}

// ----------------------------

// Add is the $add operator.
type Add []any

var _ bson.Marshaler = Add{}

func (a Add) D() bson.D {
	return bson.D{{"$add", bson.A(a)}}
}

func (a Add) MarshalBSON() ([]byte, error) {
	return bson.Marshal(a.D())
}

// ----------------------------

// Mod is the $mod operator.
type Mod [2]any

var _ bson.Marshaler = Mod{}

func (m Mod) D() bson.D {
	return bson.D{{"$mod", bson.A(m[:])}}
}

func (m Mod) MarshalBSON() ([]byte, error) {
	return bson.Marshal(m.D())
}

// ----------------------------

// Subtract is the $subtract operator.
type Subtract [2]any

var _ bson.Marshaler = Subtract{}

func (s Subtract) D() bson.D {
	return bson.D{{"$subtract", bson.A(s[:])}}
}

func (s Subtract) MarshalBSON() ([]byte, error) {
	return bson.Marshal(s.D())
}
