package mmongo

import (
	"fmt"
	"net/url"
	"strings"

	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/connstring"
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
			uri, err := url.ParseRequestURI(in)
			if err != nil {
				return false, "", fmt.Errorf("parsing connection string (%s) as URI: %w", in, err)
			}

			if !strings.HasSuffix(uri.Path, "/") {
				uri.Path += "/"
			}

			query := uri.Query()
			query.Add("directConnection", "true")
			uri.RawQuery = query.Encode()

			in = uri.String()

			added = true
		}
	}

	return added, in, nil
}
