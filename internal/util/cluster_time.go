package util

import (
	"github.com/10gen/migration-verifier/mbson"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// ErrSessionHasNoClusterTime is returned by GetClusterTimeFromSession when the
// underlying session never received a cluster time from the server. Callers
// that have a sensible fallback (e.g. wall-clock for CosmosDB) should handle
// this rather than treating it as fatal.
var ErrSessionHasNoClusterTime = errors.New("session has no cluster time")

func GetClusterTimeFromSession(sess *mongo.Session) (bson.Timestamp, error) {
	clusterTimeRaw := sess.ClusterTime()

	if clusterTimeRaw == nil {
		return bson.Timestamp{}, ErrSessionHasNoClusterTime
	}

	ctrv, err := clusterTimeRaw.LookupErr("$clusterTime", "clusterTime")
	if err != nil {
		return bson.Timestamp{}, errors.Wrapf(err, "finding clusterTime in session cluster time document (%v)", clusterTimeRaw)
	}

	return mbson.CastRawValue[bson.Timestamp](ctrv)
}
