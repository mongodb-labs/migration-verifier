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
func (verifier *MockVerifier) WritesOff(ctx context.Context) {}
func (verifier *MockVerifier) WritesOn(ctx context.Context)  {}
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
		"filter": {"i": {"$gt": 10 } }
	}`

	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/api/v1/check", strings.NewReader(input))
	suite.Require().NoError(err)

	router.ServeHTTP(w, req)
	suite.Require().Equal(200, w.Code)
	suite.Require().Equal(suite.mockVerifier.filter, bson.D{{"i", bson.D{{"$gt", int64(10)}}}})
}

func TestWebServer(t *testing.T) {
	testSuite := NewWebServerSuite()
	suite.Run(t, testSuite)
}
