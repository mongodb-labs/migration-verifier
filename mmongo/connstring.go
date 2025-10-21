package mmongo

import (
	"fmt"

	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// MaybeAddDirectConnection adds the `directConnection` parameter
// to the connection string if:
//   - There is only 1 host.
//   - The connection string lacks parameters that contraindicate a
//     direct connection.
//
// This logic mimics mongosh’s behavior. See:
// https://github.com/mongodb-js/mongosh/blob/fea739edfa86edc2a60756d9a9d478f87d94ddda/packages/arg-parser/src/uri-generator.ts#L308
func MaybeAddDirectConnection(in string) (bool, string, error) {
	opts := options.Client().ApplyURI(in)
	if err := opts.Validate(); err != nil {
		return false, "", errors.Wrapf(err, "parsing connection string %#q", in)
	}

	var added bool

	switch len(opts.Hosts) {
	case 0:
		return false, "", fmt.Errorf("connection string has no hosts?? (%#q)", in)
	case 1:
		if opts.ReplicaSet == nil && opts.Direct == nil && opts.LoadBalanced == nil {
			opts.Direct = lo.ToPtr(true)
			added = true
		}
	}

	return added, opts.GetURI(), nil
}
