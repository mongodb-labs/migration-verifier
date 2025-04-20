package verifier

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mongodb-labs/migration-verifier/internal/logger"
	"github.com/stretchr/testify/suite"
)

// WebServerTestSuite uses a mock verifier to test webserver endpoints.
type WebServerTestSuite struct {
	suite.Suite
	webServer    *WebServer
	mockVerifier *MockVerifier
}

type MockVerifier struct {
	filter map[string]any
}

func NewMockVerifier() *MockVerifier {
	return &MockVerifier{}
}

func (verifier *MockVerifier) Check(ctx context.Context, filter map[string]any) {
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
		"filter": {"i": {"$gt": 10 } }
	}`

	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/api/v1/check", strings.NewReader(input))
	suite.Require().NoError(err)

	router.ServeHTTP(w, req)
	suite.Require().Equal(200, w.Code)
	suite.Require().Equal(map[string]any{"i": map[string]any{"$gt": json.Number("10")}}, suite.mockVerifier.filter)

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
