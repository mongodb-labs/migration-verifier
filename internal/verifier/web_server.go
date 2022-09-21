package verifier

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"golang.org/x/sync/semaphore"
)

// RequestInProgressErrorDescription is the error description for RequestInProgressError
const RequestInProgressErrorDescription = "Another request is currently in progress"

// MigrationVerifierAPI represents the interaction webserver with mongosync
type MigrationVerifierAPI interface {
	Check(ctx context.Context)
	WritesOff(ctx context.Context)
	WritesOn(ctx context.Context)
	GetProgress(ctx context.Context) (Progress, error)
}

// WebServer represents the HTTP server
type WebServer struct {
	port               int
	Mapi               MigrationVerifierAPI
	logger             *logger.Logger
	srv                *http.Server
	operationalAPILock *semaphore.Weighted
	signalShutdown     context.CancelFunc
	mongosyncError     error
}

// APIResponse is the schema for Operational API response
type APIResponse struct {
	Success          bool    `json:"success"`
	Error            *string `json:"error,omitempty"`
	ErrorDescription *string `json:"errorDescription,omitempty"`
}

// NewWebServer creates a WebServer object
func NewWebServer(port int, mapi MigrationVerifierAPI, logger *logger.Logger) *WebServer {
	return &WebServer{
		port:               port,
		Mapi:               mapi,
		logger:             logger,
		operationalAPILock: semaphore.NewWeighted(1),
	}
}

func (server *WebServer) operationalAPILockMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !server.operationalAPILock.TryAcquire(1) {
			err := fmt.Errorf("Request in progress")
			server.operationalErrorResponse(c, err)
			c.Abort()
			return
		}
		defer server.operationalAPILock.Release(1)
		c.Next()
	}
}

// A wrapper around gin.ResponseWriter with its own buffer.
// This lets us capture the response body and log it separately.
type responseBodyWriter struct {
	gin.ResponseWriter
	body *bytes.Buffer
}

// Write stores the provided bytes before calling (gin.ResponseWriter).Write.
func (rbw responseBodyWriter) Write(b []byte) (int, error) {
	rbw.body.Write(b)
	return rbw.ResponseWriter.Write(b)
}

// RequestAndResponseLogger is the middleware for logging the request and response.
func (server *WebServer) RequestAndResponseLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		t := time.Now()

		// A UUID to correlate each request with a response in the logs.
		traceID := uuid.New().String()

		var buf []byte
		if c.Request.Body != nil {
			// The request body can only be read once.
			buf, _ = io.ReadAll(c.Request.Body)
		}
		server.logger.Info().Str("uri", c.Request.RequestURI).
			Str("method", c.Request.Method).
			Str("body", string(buf)).
			Str("clientIP", c.ClientIP()).
			Str("traceID", traceID).
			Msg("received request")

		// Reinstate the request body.
		c.Request.Body = io.NopCloser(bytes.NewBuffer(buf))

		// Add the UUID to the header.
		c.Header("Trace-Id", traceID)

		// Capture the response body and log it separately below.
		rbw := &responseBodyWriter{ResponseWriter: c.Writer, body: bytes.NewBufferString("")}
		c.Writer = rbw

		c.Next()

		server.logger.Info().Int("status", c.Writer.Status()).
			Str("body", rbw.body.String()).
			Str("traceID", traceID).
			Str("latency", time.Since(t).String()).
			Msg("sent response")
	}
}

// Run checks the web server. This is a blocking call.
// This function should only be called once during each Webserver's life time.
func (server *WebServer) Run(ctx context.Context) error {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(server.RequestAndResponseLogger(), gin.Recovery())

	api := router.Group("/api")
	{
		v1 := api.Group("/v1")
		{
			v1.POST("/check", server.operationalAPILockMiddleware(), server.checkEndPoint)
			v1.POST("/writesOff", server.operationalAPILockMiddleware(), server.writesOffEndpoint)
			v1.GET("/progress", server.progressEndpoint)
		}
	}

	router.HandleMethodNotAllowed = true

	server.srv = &http.Server{
		Addr:    "0.0.0.0:" + strconv.Itoa(server.port),
		Handler: router,
	}

	webServerCtx, shutDownWebServer := context.WithCancel(ctx)
	server.logger.Info().Int("port", server.port).Msg("Running webserver.")
	server.signalShutdown = shutDownWebServer

	go func() {
		// Handle incoming requests. We always get a non-nil error at the end.
		err := server.srv.ListenAndServe()

		// Shut down the server manually if needed.
		if !errors.Is(err, http.ErrServerClosed) {
			server.logger.Error().Err(err).Msg("Web server failed check")
			shutDownWebServer()
		}
	}()

	<-webServerCtx.Done()
	if err := server.srv.Shutdown(context.Background()); err != nil {
		// Web Server wasn't gracefully shutdown
		server.logger.Error().Err(err).Msg("Web server forced to shutdown")
	}

	if server.mongosyncError != nil {
		// Web Server shutdown because of MigrationVerifier error
		server.logger.Error().Err(server.mongosyncError).Msg("Web server exit because of MigrationVerifier error")
		return server.mongosyncError
	}
	return nil
}

// EmptyRequest is for request with empty body
type EmptyRequest struct{}

func (server *WebServer) checkEndPoint(c *gin.Context) {
	var json EmptyRequest

	if err := c.ShouldBindJSON(&json); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	server.Mapi.Check(context.Background())
	//if err != nil {
	//	server.operationalErrorResponse(c, err)
	//	return
	//}
	successResponse(c)
}

func (server *WebServer) writesOffEndpoint(c *gin.Context) {
	var json EmptyRequest

	if err := c.ShouldBindJSON(&json); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	server.Mapi.WritesOff(context.Background())
	successResponse(c)
}

// Progress represents the structure of the JSON response from the Progress end point.
type Progress struct {
	Phase      string              `json:"phase"`
	Generation int                 `json:"generation"`
	Error      error               `json:"error"`
	Status     *VerificationStatus `json:"verificationStatus"`
}

// progressEndpoint implements the gin handle for the progress endpoint.
func (server *WebServer) progressEndpoint(c *gin.Context) {
	progress, err := server.Mapi.GetProgress(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"progress": progress,
	})
}

func successResponse(c *gin.Context) {
	c.JSON(http.StatusOK, APIResponse{true, nil, nil})
}

func (server *WebServer) operationalErrorResponse(c *gin.Context, err error) {
	errorName := "APIError"

	server.logger.Error().Err(err).Msg("Un-recoverable error during operational API handler, shutting down.")
	server.mongosyncError = err
	server.signalShutdown()

	errorDescription := err.Error()
	c.JSON(http.StatusOK, APIResponse{false, &errorName, &errorDescription})
}
