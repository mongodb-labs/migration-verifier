package verifier

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildClientOpts_HTTPClient guards against a regression that segfaulted
// the driver: options built via a &options.ClientOptions{} literal leave
// HTTPClient nil, and mongo.Connect passes that field straight to
// auth.CreateAuthenticator with no nil fallback. The resulting
// OIDCAuthenticator then nil-dereferences it in its Azure/GCP callbacks
// (x/mongo/driver/auth/oidc.go, httpClient.Do), panicking inside the
// connection pool rather than returning an error.
func TestBuildClientOpts_HTTPClient(t *testing.T) {
	verifier := &Verifier{readConcernSetting: ReadConcernMajority}

	for _, uri := range []string{
		"mongodb://localhost:27017",

		// The mechanism that actually dereferences HTTPClient.
		"mongodb://localhost:27017/?authMechanism=MONGODB-OIDC" +
			"&authMechanismProperties=ENVIRONMENT:gcp,TOKEN_RESOURCE=some-resource",
	} {
		t.Run(uri, func(t *testing.T) {
			opts := verifier.buildClientOpts(uri, nil)

			require.NotNil(t, opts)
			assert.NotNil(
				t,
				opts.HTTPClient,
				"HTTPClient must be non-nil; a nil one panics the OIDC authenticator",
			)
		})
	}
}
