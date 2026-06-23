package comparehashed

import (
	"github.com/mongodb-labs/migration-tools/option"
)

// MinNextVersion returns the nearest server version that supports
// hashed-index-key document comparison. Returns None if the given version
// already supports it.
func MinNextVersion(version []int) option.Option[[]int] {
	none := option.None[[]int]()

	if version[0] >= 8 {
		return none
	}

	// NB: The following assumes minor==0. It’s not worth
	// panicking on if that’s not true, though.

	switch version[0] {
	case 7:
		if version[2] >= 6 {
			return none
		}
		return option.Some([]int{7, 0, 6})
	case 6:
		if version[2] >= 14 {
			return none
		}
		return option.Some([]int{6, 0, 14})
	case 5:
		if version[2] >= 25 {
			return none
		}
		return option.Some([]int{5, 0, 25})
	case 4:
		if version[1] == 4 && version[2] >= 29 {
			return none
		}
		return option.Some([]int{4, 4, 29})
	default:
		return option.Some([]int{4, 4, 29})
	}
}
