package buildvar

import "fmt"

const (
	buildVarDefaultStr = "Unknown; build with build.sh."
)

// These get set at build time, assuming use of build.sh.
var Revision = buildVarDefaultStr
var BuildTime = buildVarDefaultStr

func GetClientAppName() string {
	return fmt.Sprintf("Migration Verifier %s", Revision)
}
