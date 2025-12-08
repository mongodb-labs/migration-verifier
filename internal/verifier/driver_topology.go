package verifier

import "go.mongodb.org/mongo-driver/v2/x/mongo/driver/topology"

func init() {

	// Version 2.4 of the driver dropped support for server 4.0.
	// This is a workaround from GODRIVER-3701 that the maintainers
	// indicate will preserve the needed functionality.
	topology.MinSupportedMongoDBVersion = "4.0"
	topology.SupportedWireVersions.Min = 7 // 4.0
}
