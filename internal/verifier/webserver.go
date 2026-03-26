package verifier

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/10gen/migration-verifier/buildvar"
	"github.com/10gen/migration-verifier/chanutil"
	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/verifier/api"
	"github.com/10gen/migration-verifier/internal/verifier/webserver"
	"github.com/ccoveille/go-safecast/v2"
	"github.com/gin-contrib/pprof"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/mongodb-labs/migration-tools/future"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"golang.org/x/sync/semaphore"
)

// RequestInProgressErrorDescription is the error description for RequestInProgressError
const RequestInProgressErrorDescription = "Another request is currently in progress"

// WebServer represents the HTTP server
type WebServer struct {
	port               int
	Mapi               api.MigrationVerifierAPI
	logger             *logger.Logger
	srv                *http.Server
	operationalAPILock *semaphore.Weighted
	signalShutdown     context.CancelCauseFunc
	mongosyncError     error
}

// NewWebServer creates a WebServer object
func NewWebServer(port int, mapi api.MigrationVerifierAPI, logger *logger.Logger) *WebServer {
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
			err := fmt.Errorf("request in progress")
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
// Paths passed in skipResponseBodyPaths will have their response body omitted from
// the log (e.g. streaming endpoints that can return arbitrarily large payloads).
func (server *WebServer) RequestAndResponseLogger(skipResponseBodyPaths ...string) gin.HandlerFunc {
	skipSet := make(map[string]bool, len(skipResponseBodyPaths))
	for _, p := range skipResponseBodyPaths {
		skipSet[p] = true
	}

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

		var rbw *responseBodyWriter

		if !skipSet[c.FullPath()] {
			// Capture the response body and log it separately below.
			rbw = &responseBodyWriter{ResponseWriter: c.Writer, body: bytes.NewBufferString("")}
			c.Writer = rbw
		}

		c.Next()

		event := server.logger.Info().Int("status", c.Writer.Status()).
			Int("bytesWritten", c.Writer.Size()).
			Str("traceID", traceID).
			Str("latency", time.Since(t).String())

		if rbw != nil {
			event = event.Str("body", rbw.body.String())
		}

		event.Msg("sent response")
	}
}

func (server *WebServer) setupRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	// This is set so that gin does not decode JSON numbers into float64.
	// The actual BSON type conversion for JSON numbers is deferred to go-driver.
	gin.EnableJsonDecoderUseNumber()

	router := gin.New()
	pprof.Register(router)
	router.Use(server.RequestAndResponseLogger(
		"/api/v1/docMismatches",
		"/api/v1/nsMismatches",
	), gin.Recovery())

	router.Use(func(c *gin.Context) {
		c.Header("Server", "migration-verifier/"+buildvar.Revision)
		c.Next()
	})

	api := router.Group("/api")
	{
		v1 := api.Group("/v1")
		{
			v1.POST("/check", server.operationalAPILockMiddleware(), server.checkEndPoint)
			v1.POST("/writesOff", server.operationalAPILockMiddleware(), server.writesOffEndpoint)
			v1.GET("/progress", server.progressEndpoint)
			v1.GET("/docMismatches", server.docMismatchesEndpoint)
			v1.GET("/nsMismatches", server.nsMismatchesEndpoint)
		}
	}

	router.HandleMethodNotAllowed = true
	return router
}

// Run checks the web server. This is a blocking call.
// This function should only be called once during each Webserver's life time.
func (server *WebServer) Run(ctx context.Context) error {
	addrStr := fmt.Sprintf("0.0.0.0:%d", server.port)
	server.logger.Info().
		Str("address", addrStr).
		Msg("Starting web server.")

	listener, err := net.Listen("tcp", addrStr)
	if err != nil {
		return errors.Wrapf(err, "failed to bind to %s", addrStr)
	}

	boundPort := listener.Addr().(*net.TCPAddr).Port
	server.logger.Info().
		Int("port", boundPort).
		Msg("Web server started.")

	server.srv = &http.Server{
		Handler: server.setupRouter(),
	}

	webServerCtx, shutDownWebServer := contextplus.WithCancelCause(ctx)

	server.signalShutdown = shutDownWebServer

	go func() {
		// Handle incoming requests. We always get a non-nil error at the end.
		err := server.srv.Serve(listener)

		// Shut down the server manually if needed.
		if !errors.Is(err, http.ErrServerClosed) {
			server.logger.Error().Err(err).Msg("Web server failed check")
			shutDownWebServer(err)
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

// CheckRequest is for requests to the /check endpoint.
type CheckRequest struct {
	Filter bson.D `bson:"filter"`
}

func (server *WebServer) checkEndPoint(c *gin.Context) {
	var req CheckRequest

	if err := c.ShouldBindWith(&req, webserver.ExtJSONBinding); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	server.Mapi.Check(context.Background(), req.Filter)
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

	err := server.Mapi.WritesOff(context.Background())
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"error": err.Error()})
		return
	}
	successResponse(c)
}

// progressEndpoint implements the gin handle for the progress endpoint.
func (server *WebServer) progressEndpoint(c *gin.Context) {
	var payload []byte

	progress, err := server.Mapi.GetProgress(c.Request.Context())

	if err == nil {
		payload, err = bson.MarshalExtJSON(
			bson.M{"progress": progress},
			false, // relaxed
			false, // no HTML escape
		)
	}

	if err != nil {
		c.JSON(http.StatusOK, gin.H{"error": err.Error()})
		return
	}

	c.Data(http.StatusOK, "application/json", payload)
}

func (server *WebServer) docMismatchesEndpoint(c *gin.Context) {
	const minDurationSecsKey = "minDurationSecs"

	var minDurationSecs uint64
	if val := c.Query(minDurationSecsKey); val != "" {
		var err error
		minDurationSecs, err = strconv.ParseUint(val, 10, 32)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("invalid %#q", minDurationSecsKey),
			})
			return
		}
	}

	serveMismatches(
		c,
		server,
		func(
			ctx context.Context,
			mmChan chan<- api.DocMismatchInfo,
			errSetter future.Setter[error],
		) {
			err := server.Mapi.SendDocumentMismatches(
				ctx,
				safecast.MustConvert[uint32](minDurationSecs),
				mmChan,
			)

			errSetter(err)
		},
	)
}

func (server *WebServer) nsMismatchesEndpoint(c *gin.Context) {
	const ignoreKey = "indexSpecIgnore"

	var specIgnore []api.IndexSpecTolerance

	if val := c.Query(ignoreKey); val != "" {
		specIgnore = lo.Map(
			strings.Split(val, ","),
			func(in string, _ int) api.IndexSpecTolerance {
				return api.IndexSpecTolerance(in)
			},
		)

		invalid := lo.Filter(
			specIgnore,
			func(piece api.IndexSpecTolerance, _ int) bool {
				return !slices.Contains(api.IndexMismatchTolerances(), piece)
			},
		)

		if len(invalid) > 0 {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("invalid %#q: %#q", ignoreKey, invalid),
			})
			return
		}
	}

	serveMismatches(
		c,
		server,
		func(
			ctx context.Context,
			mmChan chan<- api.NSMismatchInfo,
			errSetter future.Setter[error],
		) {
			err := server.Mapi.SendNamespaceMismatches(
				ctx,
				specIgnore,
				mmChan,
			)

			errSetter(err)
		},
	)
}

func serveMismatches[T api.AnyMismatchInfo](
	c *gin.Context,
	server *WebServer,
	sender func(
		context.Context,
		chan<- T,
		future.Setter[error],
	),
) {
	c.Header("Content-Type", "application/x-ndjson")

	// Prevent proxies/load balancers from buffering the response
	c.Header("X-Accel-Buffering", "no")
	c.Header("Cache-Control", "no-cache")

	mmChan := make(chan T)

	senderCtx, senderCancel := contextplus.WithCancelCause(c)
	defer senderCancel(fmt.Errorf("OK"))

	mmErr, errSetter := future.New[error]()
	go sender(senderCtx, mmChan, errSetter)

	errEncoder := json.NewEncoder(c.Writer)

	sendError := func(err error) {
		// Prefer the trace ID, which should be set. Just in case, though,
		// we create our own ID for the error.
		errRef := cmp.Or(
			c.Writer.Header().Get("Trace-Id"),
			strconv.FormatUint(rand.Uint64(), 16),
		)

		server.logger.Error().
			Str("request", c.Request.URL.Path).
			Str("ref", errRef).
			Err(err).
			Msgf("Internal error.")

		payload := gin.H{
			"error": fmt.Sprintf("internal error (ref #: %s)", errRef),
		}

		// Skip error checking.
		_ = errEncoder.Encode(payload)
	}

READ:
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-mmErr.Ready():
			break READ
		case mismatch, open := <-mmChan:
			if !open {
				break READ
			}

			ejson, err := bson.MarshalExtJSON(mismatch, false, false)
			if err != nil {
				senderCancel(fmt.Errorf("marshal ext json: %w", err))
				sendError(err)
				return
			}

			ejson = append(ejson, '\n')

			if _, err := c.Writer.Write(ejson); err != nil {
				server.logger.Error().
					Err(err).
					Msg("sending mismatch")

				return
			}
		}
	}

	_, err := chanutil.ReadWithDoneCheck(c, mmErr.Ready())
	if err != nil {
		return
	}

	if err := mmErr.Get(); err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			fallthrough
		case errors.Is(err, context.DeadlineExceeded):
		default:
			sendError(err)
		}
	}
}

func successResponse(c *gin.Context) {
	c.JSON(http.StatusOK, api.Response{true, nil, nil})
}

func (server *WebServer) operationalErrorResponse(c *gin.Context, err error) {
	errorName := "APIError"

	server.logger.Error().Err(err).Msg("Un-recoverable error during operational API handler, shutting down.")
	server.mongosyncError = err
	server.signalShutdown(err)

	errorDescription := err.Error()
	c.JSON(http.StatusOK, api.Response{false, &errorName, &errorDescription})
}
