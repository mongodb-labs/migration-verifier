package verifier

import "go.mongodb.org/mongo-driver/v2/x/mongo/driver/topology"

func init() {
	topology.MinSupportedMongoDBVersion = "4.0"

	topology.SupportedWireVersions.Min = 7 // 4.0
}
