package mmongo

import (
	"net/url"

	"github.com/10gen/migration-verifier/buildvar"
	"github.com/mongodb-labs/migration-tools/mongotools"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// GetDirectClient creates a direct connection by parsing a connection
// string as a URI and replacing its authority section. This preserves any
// options in the connection string, including authentication.
//
// This is useful if, e.g., you need to pin requests to a particular node
// in a replica set.
func GetDirectClient(
	baseConnstr string,
	hostnameAndPort string,
) (*mongo.Client, error) {
	connstr, err := setDirectHostInConnectionString(
		baseConnstr,
		hostnameAndPort,
	)
	if err != nil {
		return nil, errors.Wrapf(err, "setting source connstr to connect directly to %#q", hostnameAndPort)
	}

	client, err := mongo.Connect(options.Client().
		ApplyURI(connstr).
		SetAppName(buildvar.GetClientAppName()),
	)
	if err != nil {
		return nil, errors.Wrapf(err, "connecting directly to source %#q", hostnameAndPort)
	}

	return client, err
}

func setDirectHostInConnectionString(connstr, hostname string) (string, error) {
	parsedURI, err := url.ParseRequestURI(connstr)
	if err != nil {
		return "", errors.Wrapf(err, "parsing connection string")
	}

	parsedURI.Host = hostname

	_, connstr, err = mongotools.MaybeAddDirectConnection(parsedURI.String())
	if err != nil {
		return "", errors.Wrapf(err, "tweaking connection string to %#q to ensure direct connection", parsedURI.Host)
	}

	return connstr, nil
}
