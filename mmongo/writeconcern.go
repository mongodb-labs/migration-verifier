package mmongo

import (
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

var WriteConcernMajorityRaw = lo.Must(bson.Marshal(bson.D{
	{"w", "majority"},
}))
