package verifier

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/stretchr/testify/suite"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// WebServerTestSuite uses a mock verifier to test webserver endpoints.
type WebServerTestSuite struct {
	suite.Suite
	webServer    *WebServer
	mockVerifier *MockVerifier
}

type MockVerifier struct {
	filter bson.D
}

func NewMockVerifier() *MockVerifier {
	return &MockVerifier{}
}

func (verifier *MockVerifier) Check(ctx context.Context, filter bson.D) {
	verifier.filter = filter
}
func (verifier *MockVerifier) WritesOff(ctx context.Context) error { return nil }
func (verifier *MockVerifier) WritesOn(ctx context.Context)        {}
func (verifier *MockVerifier) GetProgress(ctx context.Context) (Progress, error) {
	return Progress{}, nil
}

func NewWebServerSuite() *WebServerTestSuite {
	mv := NewMockVerifier()
	return &WebServerTestSuite{
		mockVerifier: mv,
		webServer:    NewWebServer(27020, mv, logger.NewDebugLogger()),
	}
}

func (suite *WebServerTestSuite) TestCheckEndPoint() {
	router := suite.webServer.setupRouter()

	input := `{
		"filter": {
			"i": {"$gt": 10 },
			"$and": [
				{"_id": {"$gte": { "$oid": "507f1f77bcf86cd799439000" }}},
				{"_id": {"$lte": { "$oid": "507f1f77bcf86cd7994390ff" }}}
			]
		}
	}`

	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/api/v1/check", strings.NewReader(input))
	suite.Require().NoError(err)

	router.ServeHTTP(w, req)
	suite.Require().Equal(200, w.Code)
	suite.Require().Equal(
		bson.D{
			{"i", bson.D{
				{"$gt", int32(10)},
			}},
			{"$and", bson.A{
				bson.D{
					{"_id", bson.D{{"$gte", primitive.ObjectID{0x50, 0x7f, 0x1f, 0x77, 0xbc, 0xf8, 0x6c, 0xd7, 0x99, 0x43, 0x90, 0x00}}}},
				},
				bson.D{
					{"_id", bson.D{{"$lte", primitive.ObjectID{0x50, 0x7f, 0x1f, 0x77, 0xbc, 0xf8, 0x6c, 0xd7, 0x99, 0x43, 0x90, 0xff}}}},
				},
			}},
		},
		suite.mockVerifier.filter,
		"filter should be parsed correctly",
	)

	invalidJSONInput1 := `{
		"filter": "A string, not JSON."
	}`
	w = httptest.NewRecorder()
	req, err = http.NewRequest("POST", "/api/v1/check", strings.NewReader(invalidJSONInput1))
	suite.Require().NoError(err)

	router.ServeHTTP(w, req)
	suite.Require().Equal(400, w.Code)
	suite.Require().Contains(w.Body.String(), "error")

	invalidJSONInput2 := `{
		"filter": {
	}`
	w = httptest.NewRecorder()
	req, err = http.NewRequest("POST", "/api/v1/check", strings.NewReader(invalidJSONInput2))
	suite.Require().NoError(err)

	router.ServeHTTP(w, req)
	suite.Require().Equal(400, w.Code)
	suite.Require().Contains(w.Body.String(), "error")
}

func TestWebServer(t *testing.T) {
	testSuite := NewWebServerSuite()
	suite.Run(t, testSuite)
}
