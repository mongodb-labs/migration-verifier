package expr

import "go.mongodb.org/mongo-driver/v2/bson"

// Cond is the $cond operator.
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

// Switch is the $switch operator. Default is optional; it is omitted
// from the expression when nil.
type Switch struct {
	Branches []SwitchCase
	Default  any
}

var _ bson.Marshaler = Switch{}

// SwitchCase is one branch of a $switch expression.
type SwitchCase struct {
	Case any
	Then any
}

func (s Switch) D() bson.D {
	spec := bson.D{{"branches", s.Branches}}
	if s.Default != nil {
		spec = append(spec, bson.E{"default", s.Default})
	}

	return bson.D{{"$switch", spec}}
}

func (s Switch) MarshalBSON() ([]byte, error) {
	return bson.Marshal(s.D())
}

// ---------------------------------------------

// IfNull is the $ifNull operator.
type IfNull []any

var _ bson.Marshaler = IfNull{}

func (i IfNull) D() bson.D {
	return bson.D{{"$ifNull", bson.A(i)}}
}

func (i IfNull) MarshalBSON() ([]byte, error) {
	return bson.Marshal(i.D())
}

// ---------------------------------------------

// Nor is the $nor operator.
type Nor []any

var _ bson.Marshaler = Nor{}

func (n Nor) D() bson.D {
	return bson.D{{"$nor", bson.A(n)}}
}

func (n Nor) MarshalBSON() ([]byte, error) {
	return bson.Marshal(n.D())
}
