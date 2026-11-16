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
	"github.com/mongodb-labs/migration-tools/option"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"golang.org/x/sync/semaphore"
)

// RequestInProgressErrorDescription is the RequestInProgress error’s description.
const (
	RequestInProgressErrorDescription = "Another request is currently in progress"
	shutdownTimeout                   = time.Minute
	minDurationSecsKey                = "minDurationSecs"
)

// WebServer represents the HTTP server
type WebServer struct {
	port               int
	Mapi               api.MigrationVerifierAPI
	logger             *logger.Logger
	srv                *http.Server
	operationalAPILock *semaphore.Weighted

	// metricsHandler, if set, is served at /metrics.
	metricsHandler option.Option[http.Handler]
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
		// Turn the request away, but keep the server running: the request
		// in progress is fine.
		if !server.operationalAPILock.TryAcquire(1) {
			errorName := "RequestInProgress"
			errorDescription := RequestInProgressErrorDescription
			c.AbortWithStatusJSON(
				http.StatusConflict,
				api.Response{Success: false, Error: &errorName, ErrorDescription: &errorDescription},
			)

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

// setupRouter sets up the Gin router with all the necessary routes and middleware.
// The cancelFunc cancels the server context.
func (server *WebServer) setupRouter(ctx context.Context, cancelFunc context.CancelCauseFunc) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)

	// This is set so that gin does not decode JSON numbers into float64.
	// The actual BSON type conversion for JSON numbers is deferred to go-driver.
	gin.EnableJsonDecoderUseNumber()

	router := gin.New()
	pprof.Register(router)

	// Like the pprof routes, /metrics is registered before the middleware
	// below, so scrapes aren’t logged. A typical 15-second scrape interval
	// would otherwise fill the log.
	if handler, has := server.metricsHandler.Get(); has {
		router.GET("/metrics", gin.WrapH(handler))
	}
	router.Use(server.RequestAndResponseLogger(
		"/api/v1/docMismatches",
		"/api/v1/nsMismatches",
		"/api/v1/summary",
	), gin.Recovery())

	router.Use(func(c *gin.Context) {
		c.Header("Server", "migration-verifier/"+buildvar.Revision)
		c.Next()
	})

	api := router.Group("/api")
	{
		v1 := api.Group("/v1")
		{
			// This endpoint is special because it doesn’t block. Its handler
			// thus needs both the server context and the request handle.
			v1.POST("/check", server.operationalAPILockMiddleware(), server.makeCheckEndPoint(ctx, cancelFunc))

			v1.POST("/writesOff", server.operationalAPILockMiddleware(), server.writesOffEndpoint)
			v1.GET("/progress", server.progressEndpoint)
			v1.GET("/summary", server.summaryEndpoint)
			v1.GET("/docMismatches", server.docMismatchesEndpoint)
			v1.GET("/nsMismatches", server.nsMismatchesEndpoint)
		}
	}

	router.HandleMethodNotAllowed = true
	return router
}

// Run starts the web server. This is a blocking call.
// This function should only be called once during each Webserver's life time.
func (server *WebServer) Run(ctx context.Context, startCheck bool) error {
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

	webServerCtx, cancelWebServer := contextplus.WithCancelCause(ctx)

	server.srv = &http.Server{
		Handler: server.setupRouter(webServerCtx, cancelWebServer),
	}

	if startCheck {
		err := server.Mapi.StartCheck(webServerCtx, cancelWebServer, nil)

		// Since we just started the server there should be no conflict,
		// but we might as well check anyhow.
		if err != nil {
			err := errors.Wrap(err, "starting check")

			server.shutDown(err)

			return err
		}
	}

	go func() {
		// Handle incoming requests. We always get a non-nil error at the end.
		err := server.srv.Serve(listener)

		// Shut down the server manually if needed.
		if !errors.Is(err, http.ErrServerClosed) {
			server.logger.Error().Err(err).Msg("Web server failed.")

			cancelWebServer(err)
		}
	}()

	<-webServerCtx.Done()

	serverCtxErr := context.Cause(webServerCtx)

	server.shutDown(serverCtxErr)

	return serverCtxErr
}

func (server *WebServer) shutDown(reason error) {
	ctx, cancel := contextplus.WithTimeoutCause(
		context.Background(),
		shutdownTimeout,
		fmt.Errorf("web server shutdown timed out"),
	)
	defer cancel()

	server.logger.Info().
		AnErr("reason", reason).
		Msg("Web server shutting down.")

	if err := server.srv.Shutdown(ctx); err != nil {
		// Web Server wasn't gracefully shutdown
		server.logger.Error().
			Err(err).
			Msg("Web server shutdown failed.")
	} else {
		server.logger.Info().
			Msg("Web server is shut down.")
	}
}

// EmptyRequest is for request with empty body
type EmptyRequest struct{}

// CheckRequest is for requests to the /check endpoint.
type CheckRequest struct {
	Filter bson.D `bson:"filter"`
}

func (server *WebServer) makeCheckEndPoint(ctx context.Context, cancelFunc context.CancelCauseFunc) func(c *gin.Context) {
	return func(c *gin.Context) {
		var req CheckRequest

		if err := c.ShouldBindWith(&req, webserver.ExtJSONBinding); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		err := server.Mapi.StartCheck(ctx, cancelFunc, req.Filter)
		if errors.Is(err, api.ErrCheckInProgress) {
			c.AbortWithStatusJSON(
				http.StatusConflict,
				api.Response{
					Error:            new("CheckInProgress"),
					ErrorDescription: new(err.Error()),
				},
			)

			return
		} else if err != nil {
			token := errorRef(c)

			server.logger.Error().
				Str("ref", token).
				Err(err).
				Msg("Failed to start check.")

			c.AbortWithStatusJSON(
				http.StatusInternalServerError,
				api.Response{
					Error:            new("InternalServerError"),
					ErrorDescription: new(fmt.Sprintf("See server logs (ref: %s) for details.", token)),
				},
			)

			return
		}

		successResponse(c)
	}
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

// summaryEndpoint implements the gin handler for the summary endpoint.
func (server *WebServer) summaryEndpoint(c *gin.Context) {
	minDurationSecs, err := parseMinDurationSecs(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": err.Error(),
		})
		return
	}

	summary, err := server.Mapi.GetSummary(c.Request.Context(), minDurationSecs)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"error": err.Error()})
		return
	}

	payload, err := bson.MarshalExtJSON(summary, false, false)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"error": err.Error()})
		return
	}

	c.Data(http.StatusOK, "application/json", payload)
}

func parseMinDurationSecs(c *gin.Context) (uint32, error) {
	val := c.Query(minDurationSecsKey)

	if val == "" {
		return 0, nil
	}

	minDurationSecs, err := strconv.ParseUint(val, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid %#q: %w", minDurationSecsKey, err)
	}

	return safecast.MustConvert[uint32](minDurationSecs), nil
}

func (server *WebServer) docMismatchesEndpoint(c *gin.Context) {
	minDurationSecs, err := parseMinDurationSecs(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": err.Error(),
		})
		return
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
		errRef := errorRef(c)

		server.logger.Error().
			Str("request", c.Request.URL.Path).
			Str("ref", errRef).
			Err(err).
			Msgf("Internal error.")

		payload := gin.H{
			"error": fmt.Sprintf("internal error (ref #: %s)", errRef),
		}

		if encodeErr := errEncoder.Encode(payload); encodeErr != nil {
			// If the request context is already done, the client most likely
			// disconnected; avoid noisy logging in that case.
			select {
			case <-c.Request.Context().Done():
				return
			default:
			}

			server.logger.Error().
				Str("request", c.Request.URL.Path).
				Str("ref", errRef).
				Err(encodeErr).
				Msg("failed to encode error response")
		}
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

// errorRef returns an ID to report to the client in place of an internal
// error, and to log with the error. It prefers the request’s trace ID, which
// should be set; just in case, though, it creates an ID otherwise.
func errorRef(c *gin.Context) string {
	return cmp.Or(
		c.Writer.Header().Get("Trace-Id"),
		strconv.FormatUint(rand.Uint64(), 16),
	)
}

func successResponse(c *gin.Context) {
	c.JSON(http.StatusOK, api.Response{true, nil, nil})
}
