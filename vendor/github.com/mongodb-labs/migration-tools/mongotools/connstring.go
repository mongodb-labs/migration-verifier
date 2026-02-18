package mongotools

import (
	"fmt"
	"strings"

	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/connstring"
)

// MaybeAddDirectConnection adds the `directConnection` parameter
// to the connection string if:
//   - There is only 1 host.
//   - The connection string lacks parameters that contraindicate a
//     direct connection.
//
// This mimics mongosh’s behavior. See:
// https://github.com/mongodb-js/mongosh/blob/fea739edfa86edc2a60756d9a9d478f87d94ddda/packages/arg-parser/src/uri-generator.ts#L308
func MaybeAddDirectConnection(in string) (bool, string, error) {
	cs, err := connstring.ParseAndValidate(in)

	if err != nil {
		return false, "", fmt.Errorf("parsing connection string %#q: %w", in, err)
	}

	var added bool

	switch len(cs.Hosts) {
	case 0:
		return false, "", fmt.Errorf("connection string has no hosts?? (%#q)", in)
	case 1:
		if cs.ReplicaSet == "" && !cs.DirectConnectionSet && !cs.LoadBalancedSet {
			if !strings.Contains(in, "?") {
				if cs.Database == "" {
					in += "/"
				}

				in += "?"
			}

			_, query, found := strings.Cut(in, "?")
			lo.Assertf(
				found,
				"connstr (%s) needs query separator",
				in,
			)

			if strings.Contains(query, "&") && !strings.HasSuffix(query, "&") {
				in += "&"
			}

			in += "directConnection=true"

			added = true
		}
	}

	return added, in, nil
}
