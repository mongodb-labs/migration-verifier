package expr

import "go.mongodb.org/mongo-driver/v2/bson"

// Eq is the $eq operator.
type Eq [2]any

var _ bson.Marshaler = Eq{}

func (e Eq) D() bson.D {
	return bson.D{{"$eq", bson.A(e[:])}}
}

func (e Eq) MarshalBSON() ([]byte, error) {
	return bson.Marshal(e.D())
}

// ---------------------------------------------

// Ne is the $ne operator.
type Ne [2]any

var _ bson.Marshaler = Ne{}

func (n Ne) D() bson.D {
	return bson.D{{"$ne", bson.A(n[:])}}
}

func (n Ne) MarshalBSON() ([]byte, error) {
	return bson.Marshal(n.D())
}

// ---------------------------------------------

// Gt is the $gt operator.
type Gt [2]any

var _ bson.Marshaler = Gt{}

func (g Gt) D() bson.D {
	return bson.D{{"$gt", bson.A(g[:])}}
}

func (g Gt) MarshalBSON() ([]byte, error) {
	return bson.Marshal(g.D())
}

// ---------------------------------------------

// Lt is the $lt operator.
type Lt [2]any

var _ bson.Marshaler = Lt{}

func (l Lt) D() bson.D {
	return bson.D{{"$lt", bson.A(l[:])}}
}

func (l Lt) MarshalBSON() ([]byte, error) {
	return bson.Marshal(l.D())
}

// ---------------------------------------------

// Lte is the $lte operator.
type Lte [2]any

var _ bson.Marshaler = Lte{}

func (l Lte) D() bson.D {
	return bson.D{{"$lte", bson.A(l[:])}}
}

func (l Lte) MarshalBSON() ([]byte, error) {
	return bson.Marshal(l.D())
}

// ---------------------------------------------

// Gte is the $gte operator.
type Gte [2]any

var _ bson.Marshaler = Gte{}

func (g Gte) D() bson.D {
	return bson.D{{"$gte", bson.A(g[:])}}
}

func (g Gte) MarshalBSON() ([]byte, error) {
	return bson.Marshal(g.D())
}

// ---------------------------------------------

// In is the $in operator. The variadic haystack allows literal values
// (In("x", "a", "b", "c")) as well as spreading a Go slice
// (In("x", mySlice...)).
func In[T any](needle any, haystack ...T) bson.D {
	return bson.D{{"$in", bson.A{needle, haystack}}}
}
