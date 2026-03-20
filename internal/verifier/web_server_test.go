package verifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/verifier/api"
	"github.com/10gen/migration-verifier/option"
	"github.com/mongodb-labs/migration-tools/bsontools"
	"github.com/stretchr/testify/suite"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// WebServerTestSuite uses a mock verifier to test webserver endpoints.
type WebServerTestSuite struct {
	suite.Suite
	webServer    *WebServer
	mockVerifier *MockVerifier
}

type MockVerifier struct {
	filter                  bson.D
	sendDocumentMismatches  func(context.Context, uint32, chan<- api.DocMismatchInfo) error
	sendNamespaceMismatches func(context.Context,
		chan<- api.NSMismatchInfo) error
}

func NewMockVerifier() *MockVerifier {
	return &MockVerifier{}
}

func (verifier *MockVerifier) Check(ctx context.Context, filter bson.D) {
	verifier.filter = filter
}
func (verifier *MockVerifier) WritesOff(ctx context.Context) error { return nil }
func (verifier *MockVerifier) WritesOn(ctx context.Context)        {}
func (verifier *MockVerifier) GetProgress(ctx context.Context) (api.Progress, error) {
	return api.Progress{}, nil
}

func (verifier *MockVerifier) SendDocumentMismatches(
	ctx context.Context,
	minDurationSecs uint32,
	out chan<- api.DocMismatchInfo,
) error {
	if verifier.sendDocumentMismatches == nil {
		panic("need sendDocumentMismatches set")
	}

	return verifier.sendDocumentMismatches(ctx, minDurationSecs, out)
}

func (verifier *MockVerifier) SendNamespaceMismatches(
	ctx context.Context,
	tolerances []api.IndexSpecTolerance,
	out chan<- api.NSMismatchInfo,
) error {
	if verifier.sendNamespaceMismatches == nil {
		panic("need sendNamespaceMismatches set")
	}

	return verifier.sendNamespaceMismatches(ctx, out)
}

func NewWebServerSuite() *WebServerTestSuite {
	mv := NewMockVerifier()
	return &WebServerTestSuite{
		mockVerifier: mv,
		webServer:    NewWebServer(27020, mv, logger.NewDebugLogger()),
	}
}

func (suite *WebServerTestSuite) TestDocMismatchesEndPoint_ImmediateError() {
	defer func() {
		suite.mockVerifier.sendDocumentMismatches = nil
	}()

	suite.mockVerifier.sendDocumentMismatches = func(
		ctx context.Context,
		u uint32,
		c chan<- api.DocMismatchInfo,
	) error {
		close(c)

		return fmt.Errorf("sudden error")
	}

	router := suite.webServer.setupRouter()

	w := httptest.NewRecorder()
	w.Body = bytes.NewBuffer(nil)
	req, err := http.NewRequest("GET", "/api/v1/docMismatches", nil)
	suite.Require().NoError(err)

	router.ServeHTTP(w, req)
	suite.Require().Equal(200, w.Code)

	suite.Assert().NotContains(w.Body.String(), "sudden error")
	suite.Assert().Contains(w.Body.String(), "internal error")
}

func (suite *WebServerTestSuite) TestDocMismatchesEndPoint_Success() {
	defer func() {
		suite.mockVerifier.sendDocumentMismatches = nil
	}()

	suite.mockVerifier.sendDocumentMismatches = func(
		ctx context.Context,
		u uint32,
		c chan<- api.DocMismatchInfo,
	) error {
		c <- api.DocMismatchInfo{
			Type:      api.MismatchContent,
			Namespace: "name.space1",
			ID:        bsontools.ToRawValue("onBoth"),
			Field:     option.Some("someField"),
			Detail:    option.Some("something something"),
		}

		c <- api.DocMismatchInfo{
			Type:      api.MismatchExtra,
			Namespace: "name.space2",
			ID:        bsontools.ToRawValue("onDst"),
		}

		c <- api.DocMismatchInfo{
			Type:      api.MismatchMissing,
			Namespace: "name.space3",
			ID:        bsontools.ToRawValue("onSrc"),
		}

		return nil
	}

	router := suite.webServer.setupRouter()

	w := httptest.NewRecorder()
	w.Body = bytes.NewBuffer(nil)
	req, err := http.NewRequest("GET", "/api/v1/docMismatches", nil)
	suite.Require().NoError(err)

	router.ServeHTTP(w, req)
	suite.Require().Equal(200, w.Code)

	decoder := json.NewDecoder(w.Body)

	var resp map[string]any

	suite.Require().NoError(decoder.Decode(&resp))
	suite.Assert().EqualValues(api.MismatchContent, resp["type"])
	suite.Assert().Equal("name.space1", resp["namespace"])
	suite.Assert().Equal("onBoth", resp["_id"])
	suite.Assert().Equal("someField", resp["field"])
	suite.Assert().Equal("something something", resp["detail"])

	clear(resp)

	suite.Require().NoError(decoder.Decode(&resp))
	suite.Assert().EqualValues(api.MismatchExtra, resp["type"])
	suite.Assert().EqualValues("name.space2", resp["namespace"])
	suite.Assert().EqualValues("onDst", resp["_id"])
	suite.Assert().NotContains(resp, "field")
	suite.Assert().NotContains(resp, "detail")

	clear(resp)

	suite.Require().NoError(decoder.Decode(&resp))
	suite.Assert().EqualValues(api.MismatchMissing, resp["type"])
	suite.Assert().EqualValues("name.space3", resp["namespace"])
	suite.Assert().EqualValues("onSrc", resp["_id"])
	suite.Assert().NotContains(resp, "field")
	suite.Assert().NotContains(resp, "detail")

	suite.Assert().ErrorIs(decoder.Decode(&resp), io.EOF, "should be done")
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
					{"_id", bson.D{{"$gte", bson.ObjectID{0x50, 0x7f, 0x1f, 0x77, 0xbc, 0xf8, 0x6c, 0xd7, 0x99, 0x43, 0x90, 0x00}}}},
				},
				bson.D{
					{"_id", bson.D{{"$lte", bson.ObjectID{0x50, 0x7f, 0x1f, 0x77, 0xbc, 0xf8, 0x6c, 0xd7, 0x99, 0x43, 0x90, 0xff}}}},
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
