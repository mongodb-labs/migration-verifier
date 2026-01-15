package mmongo

import (
	"context"
	"fmt"

	"github.com/10gen/migration-verifier/mbson"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// FindCanUseStartAt indicates whether the given version indicates a server
// that can safely handle the `find` command’s `$_startAt` parameter.
// (This accommodates https://jira.mongodb.org/browse/SERVER-110161.)
func FindCanUseStartAt(
	version [3]int,
) bool {
	if version[0] > 8 {
		return true
	}

	switch version[0] {
	case 7:
		return version[1] == 0 && version[2] >= 26
	case 8:
		switch version[1] {
		case 0:
			return version[2] >= 14
		case 2:
			return version[2] >= 1
		}
	}

	return false
}

// WhyFindCannotResume indicates why the server’s `find` command is
// non-resumable (i.e., when doing a natural scan). This returns nil
// if `find` is, in fact, resumable.
func WhyFindCannotResume(version [2]int) error {
	if VersionAtLeast(version[:], 4, 4) {
		return nil
	}

	return fmt.Errorf(
		"resumable scan requires MongoDB 4.4+ (this is %d.%d)",
		version[0],
		version[1],
	)
}

// VersionAtLeast returns whether the version is >= the version given
// as separate numbers.
func VersionAtLeast(version []int, nums ...int) bool {
	lo.Assertf(
		len(nums) > 0,
		"need at least a major version to check version (%v) against",
		version,
	)

	for i := range nums {
		lo.Assertf(
			len(version) >= i+1,
			"version %v is too short to compare against %v",
			version,
			nums,
		)

		if version[i] < nums[i] {
			return false
		}

		if version[i] > nums[i] {
			break
		}
	}

	return true
}

// GetVersionArray returns the server’s major, minor, & patch version.
func GetVersionArray(ctx context.Context, client *mongo.Client) ([3]int, error) {
	commandResult := client.Database("admin").RunCommand(ctx, bson.D{{"buildinfo", 1}})

	var va [3]int

	rawResp, err := commandResult.Raw()
	if err != nil {
		return va, errors.Wrapf(err, "failed to run %#q", "buildinfo")
	}

	arraySlice := []int{}

	_, err = mbson.RawLookup(rawResp, &arraySlice, "versionArray")
	if err != nil {
		return va, errors.Wrap(err, "failed to decode build info version array")
	}

	copy(va[:], arraySlice)

	return va, nil
}
