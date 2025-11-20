package util

import (
	"github.com/10gen/migration-verifier/mbson"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func GetClusterTimeFromSession(sess *mongo.Session) (bson.Timestamp, error) {
	clusterTimeRaw := sess.ClusterTime()

	if clusterTimeRaw == nil {
		panic("session has empty cluster time?!?")
	}

	ctrv, err := clusterTimeRaw.LookupErr("$clusterTime", "clusterTime")
	if err != nil {
		return bson.Timestamp{}, errors.Wrapf(err, "finding clusterTime in session cluster time document (%v)", clusterTimeRaw)
	}

	return mbson.CastRawValue[bson.Timestamp](ctrv)
}
