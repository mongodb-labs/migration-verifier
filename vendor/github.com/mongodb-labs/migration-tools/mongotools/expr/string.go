package expr

import "go.mongodb.org/mongo-driver/v2/bson"

// Concat is the $concat operator.
type Concat []any

var _ bson.Marshaler = Concat{}

func (c Concat) D() bson.D {
	return bson.D{{"$concat", bson.A(c)}}
}

func (c Concat) MarshalBSON() ([]byte, error) {
	return bson.Marshal(c.D())
}

// ----------------------------

// Split is the $split operator.
type Split [2]any

var _ bson.Marshaler = Split{}

func (s Split) D() bson.D {
	return bson.D{{"$split", bson.A(s[:])}}
}

func (s Split) MarshalBSON() ([]byte, error) {
	return bson.Marshal(s.D())
}

// ----------------------------

// SubstrBytes is the $substrBytes operator.
type SubstrBytes [3]any

var _ bson.Marshaler = SubstrBytes{}

func (s SubstrBytes) D() bson.D {
	return bson.D{{"$substrBytes", bson.A(s[:])}}
}

func (s SubstrBytes) MarshalBSON() ([]byte, error) {
	return bson.Marshal(s.D())
}

// ----------------------------

// RegexMatch is the $regexMatch operator. Options is optional; it is
// omitted from the expression when nil.
type RegexMatch struct {
	Input, Regex, Options any
}

var _ bson.Marshaler = RegexMatch{}

func (rm RegexMatch) D() bson.D {
	spec := bson.D{
		{"input", rm.Input},
		{"regex", rm.Regex},
	}
	if rm.Options != nil {
		spec = append(spec, bson.E{"options", rm.Options})
	}

	return bson.D{{"$regexMatch", spec}}
}

func (rm RegexMatch) MarshalBSON() ([]byte, error) {
	return bson.Marshal(rm.D())
}
