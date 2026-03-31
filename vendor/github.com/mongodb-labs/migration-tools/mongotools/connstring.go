package mongotools

import (
	"fmt"
	"strings"

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
//
//nolint:cyclop
func MaybeAddDirectConnection(in string) (bool, string, error) {
	cs, err := connstring.ParseAndValidate(in)

	if err != nil {
		return false, "", fmt.Errorf("parsing connection string %#q: %w", in, err)
	}

	if len(cs.Hosts) == 0 {
		return false, "", fmt.Errorf("connection string has no hosts?? (%#q)", in)
	}

	if len(cs.Hosts) > 1 || cs.ReplicaSet != "" || cs.DirectConnectionSet || cs.LoadBalancedSet {
		return false, in, nil
	}

	if !strings.Contains(in, "?") {
		if cs.Database == "" {
			in += "/"
		}
		in += "?"
	}

	_, query, _ := strings.Cut(in, "?")
	if strings.Contains(query, "&") && !strings.HasSuffix(query, "&") {
		in += "&"
	}

	return true, in + "directConnection=true", nil
}
