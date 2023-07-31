package verifier

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/suite"
	"go.mongodb.org/mongo-driver/bson"
)

type WebServerTestSuite struct {
	suite.Suite
	webServer *WebServer
}

func NewWebServerSuite() *WebServerTestSuite {
	verifier := NewVerifier(VerifierSettings{})

	return &WebServerTestSuite{
		webServer: NewWebServer(27020, verifier, verifier.logger),
	}
}

func (suite *WebServerTestSuite) TestCheckEndPoint() {
	input := `{
		"queryFilter": "{\"_id\": 0}"
	}`
	var checkRequest CheckRequest
	suite.Require().NoError(json.Unmarshal([]byte(input), &checkRequest), "json should decode")
	suite.Require().Equal(
		CheckRequest{
			Filter: "{\"_id\": 0}",
		},
		checkRequest,
		"queryFilter should unmarshal correctly",
	)

	filter, err := unmarshalJsonStringToDocument(checkRequest.Filter)
	suite.Require().NoError(err)
	suite.Require().Equal(bson.D{{"_id", int32(0)}}, filter)
}

func TestWebServer(t *testing.T) {
	testSuite := NewWebServerSuite()
	suite.Run(t, testSuite)
}
