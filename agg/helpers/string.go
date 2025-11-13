package helpers

import (
	"go.mongodb.org/mongo-driver/v2/bson"
)

type StringHasPrefix struct {
	FieldRef any
	Prefix   string
}

func (sp StringHasPrefix) MarshalBSON() ([]byte, error) {
	return bson.Marshal(bson.D{
		{"$eq", bson.A{
			0,
			bson.D{{"$indexOfCP", bson.A{
				sp.FieldRef,
				sp.Prefix,
				0,
				1,
			}}},
		}},
	})

	/*
		return bson.Marshal(agg.Eq(
			sp.Prefix,
			agg.SubstrBytes{
				sp.FieldRef,
				0,
				len(sp.Prefix),
			},
		))
	*/
}
