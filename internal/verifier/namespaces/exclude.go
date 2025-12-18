package namespaces

import (
	"github.com/10gen/migration-verifier/mslices"
)

const (
	// ExcludedSystemCollPrefix is the prefix of system collections,
	// which we ignore.
	ExcludedSystemCollPrefix = "system."

	// MongoDBInternalDBPrefix is the prefix for MongoDB-internal databases.
	// (e.g., Atlas’s availability canary)
	MongoDBInternalDBPrefix = "__mdb_internal"
)

var (
	ExcludedDBPrefixes = mslices.Of(
		"mongosync_internal_",
		"mongosync_reserved_",
		MongoDBInternalDBPrefix,
	)

	// ExcludedSystemDBs are system databases that are excluded from verification.
	ExcludedSystemDBs = []string{"admin", "config", "local"}
)
