package namespaces

import "github.com/10gen/migration-verifier/mslices"

var (
	MongosyncMetaDBPrefixes = mslices.Of(
		"mongosync_internal_",
		"mongosync_reserved_",
	)
)
