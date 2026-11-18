package main

import (
	"cmp"
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"
	"math"
	_ "net/http/pprof"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/10gen/migration-verifier/buildvar"
	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/metrics"
	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/verifier"
	"github.com/10gen/migration-verifier/internal/verifier/api"
	"github.com/10gen/migration-verifier/internal/verifier/compare"
	"github.com/10gen/migration-verifier/main/mvflags"
	"github.com/10gen/migration-verifier/msignal"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/10gen/migration-verifier/mstrings"
	"github.com/mongodb-labs/migration-tools/mongotools"
	"github.com/mongodb-labs/migration-tools/option"
	"github.com/mongodb-labs/migration-tools/sysinfo"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/rs/zerolog/pkgerrors"
	"github.com/samber/lo"
	slogzerolog "github.com/samber/slog-zerolog/v2"
	"github.com/urfave/cli"
	"github.com/urfave/cli/altsrc"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	srcURI                = "srcURI"
	dstURI                = "dstURI"
	metaURI               = "metaURI"
	numWorkers            = "numWorkers"
	generationPauseDelay  = "generationPauseDelay"
	workerSleepDelay      = "workerSleepDelay"
	serverPort            = "serverPort"
	logDir                = "logDir"
	logPathAlias          = "logPath" // deprecated name for logDir
	srcNamespace          = "srcNamespace"
	dstNamespace          = "dstNamespace"
	srcChangeReader       = "srcChangeReader"
	dstChangeReader       = "dstChangeReader"
	metaDBName            = "metaDBName"
	docCompareMethod      = "docCompareMethod"
	verifyAll             = "verifyAll"
	startClean            = "clean"
	readPreference        = "readPreference"
	partitionSizeMB       = "partitionSizeMB"
	checkOnly             = "checkOnly"
	logLevelFlag          = "logLevel"
	failureDisplaySize    = "failureDisplaySize"
	ignoreReadConcernFlag = "ignoreReadConcern"
	configFileFlag        = "configFile"
	pprofInterval         = "pprofInterval"
	startFlag             = "start"
	partitioningScheme    = "partitioningScheme"
	indexSpecIgnoreFlag   = "indexSpecIgnore"
	metricsLogDir         = "metricsLogDir"
)

const logDirDefault = "stdout"

// metricsNotice goes in the metrics flags’ help text, which is where users
// find the feature.
const metricsNotice = "Metrics are diagnostic output, not a supported interface: " +
	"anything may change or disappear in any release without notice. Metrics are never sent to any external process."

// metricsShutdownTimeout bounds the meter provider’s shutdown on exit.
const metricsShutdownTimeout = 10 * time.Second

var logLevelStrs = lo.Map(
	mslices.Of(
		zerolog.InfoLevel,
		zerolog.DebugLevel,
		zerolog.TraceLevel,
	),
	func(lv zerolog.Level, _ int) string {
		return lv.String()
	},
)

func main() {
	zerolog.ErrorStackMarshaler = pkgerrors.MarshalStack

	zerolog.SetGlobalLevel(zerolog.InfoLevel)

	ctx, stopCatchingSignals := msignal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM, // also handle termination signal
	)

	// Catch only the first signal. After it, signals get their default
	// behavior again, so a second one ends the process at once even if the
	// shutdown hangs.
	go func() {
		<-ctx.Done()
		stopCatchingSignals()
	}()

	flags := []cli.Flag{
		altsrc.NewStringFlag(cli.StringFlag{
			Name:  configFileFlag,
			Usage: "path to an optional YAML config file",
		}),
		altsrc.NewStringFlag(cli.StringFlag{
			Name:  srcURI,
			Usage: "source connection string",
		}),
		altsrc.NewStringFlag(cli.StringFlag{
			Name:  dstURI,
			Usage: "destination connection string",
		}),
		altsrc.NewStringFlag(cli.StringFlag{
			Name:  metaURI,
			Usage: "connection string to replset that stores verifier metadata (defaults to destination)",
		}),
		altsrc.NewIntFlag(cli.IntFlag{
			Name:  serverPort,
			Value: 27020,
			Usage: "`port` for the control web server (0 assigns a random port)",
		}),
		altsrc.NewStringFlag(cli.StringFlag{
			Name:  logDir,
			Value: logDirDefault,
			Usage: "`directory` for the log file (" + logger.LogFileName + "), or stdout or stderr",
		}),
		// This is a separate flag rather than an alias in logDir’s Name
		// because altsrc doesn’t read aliased flags from the config file.
		altsrc.NewStringFlag(cli.StringFlag{
			Name:   logPathAlias,
			Usage:  "deprecated name for --" + logDir,
			Hidden: true,
		}),
		altsrc.NewIntFlag(cli.IntFlag{
			Name:  numWorkers,
			Value: 10,
			Usage: "number of worker threads to use for verification",
		}),
		altsrc.NewInt64Flag(cli.Int64Flag{
			Name:  generationPauseDelay,
			Value: 1_000,
			Usage: "`milliseconds` to wait between generations of rechecking, allowing for more time to turn off writes",
		}),
		altsrc.NewInt64Flag(cli.Int64Flag{
			Name:  workerSleepDelay,
			Value: 1_000,
			Usage: "`milliseconds` workers sleep while waiting for work",
		}),
		altsrc.NewBoolFlag(cli.BoolFlag{
			Name:  verifyAll,
			Usage: "Verify all user namespaces",
		}),
		altsrc.NewStringSliceFlag(cli.StringSliceFlag{
			Name:  srcNamespace,
			Usage: "source `namespaces` to check",
		}),
		altsrc.NewStringSliceFlag(cli.StringSliceFlag{
			Name:  dstNamespace,
			Usage: "destination `namespaces` to check",
		}),
		altsrc.NewStringFlag(cli.StringFlag{
			Name:  srcChangeReader,
			Value: verifier.ChangeReaderOptChangeStream,
			Usage: "How to read changes from the source. One of: " + strings.Join(
				verifier.ChangeReaderOpts,
				", ",
			),
		}),
		altsrc.NewStringFlag(cli.StringFlag{
			Name:  dstChangeReader,
			Value: verifier.ChangeReaderOptChangeStream,
			Usage: "How to read changes from the destination. One of: " + strings.Join(
				verifier.ChangeReaderOpts,
				", ",
			),
		}),
		altsrc.NewStringFlag(cli.StringFlag{
			Name:  metaDBName,
			Value: "__mdb_internal_migration_verifier",
			Usage: "`name` of the database in which to store verification metadata",
		}),
		altsrc.NewStringFlag(cli.StringFlag{
			Name:  partitioningScheme,
			Value: partitions.SchemeDefault,
			Usage: "Method to partition documents. One of: " + strings.Join(
				partitions.Schemes,
				", ",
			),
		}),
		altsrc.NewStringFlag(cli.StringFlag{
			Name: docCompareMethod,
			Usage: "Method to compare documents. One of: " + strings.Join(
				lo.Map(
					compare.Methods,
					func(dcm compare.Method, _ int) string {
						return string(dcm)
					},
				),
				", ",
			),
			Value: string(compare.Default),
		}),
		altsrc.NewBoolFlag(cli.BoolFlag{
			Name:  startClean,
			Usage: "If set, drop all previous verification metadata before starting",
		}),
		altsrc.NewBoolFlag(cli.BoolFlag{
			Name:  startFlag,
			Usage: "Start checking documents immediately",
		}),
		altsrc.NewStringFlag(cli.StringFlag{
			Name:  readPreference,
			Value: "primary",
			Usage: "Read preference for reading data from clusters. " +
				"May be 'primary', 'secondary', 'primaryPreferred', 'secondaryPreferred', or 'nearest'",
		}),
		altsrc.NewUint64Flag(cli.Uint64Flag{
			Name:  partitionSizeMB,
			Value: 0,
			Usage: "`Megabytes` to use for a partition.  Change only for debugging. 0 means use partitioner default.",
		}),
		altsrc.NewStringFlag(cli.StringFlag{
			Name:  logLevelFlag,
			Value: zerolog.InfoLevel.String(),
			Usage: "Level of detail to include in logs. One of: " + strings.Join(logLevelStrs, ", "),
		}),
		altsrc.NewBoolFlag(cli.BoolFlag{
			Name:  checkOnly,
			Usage: "Do not run the webserver or recheck, just run the check (for debugging)",
		}),
		altsrc.NewInt64Flag(cli.Int64Flag{
			Name:  failureDisplaySize,
			Value: verifier.DefaultFailureDisplaySize,
			Usage: "Number of failures to display. Will display all failures if the number doesn’t exceed this limit by 25%",
		}),
		altsrc.NewStringSliceFlag(cli.StringSliceFlag{
			Name: indexSpecIgnoreFlag,
			Usage: "Index spec fields to tolerate mismatches on in log output. Valid values: " + strings.Join(
				mslices.Map1(
					api.IndexMismatchTolerances(),
					mstrings.ToString,
				),
				", ",
			),
		}),
		altsrc.NewBoolFlag(cli.BoolFlag{
			Name:  ignoreReadConcernFlag,
			Usage: "Use connection-default read concerns rather than setting majority read concern. This option may degrade consistency, so only enable it if majority read concern (the default) doesn’t work.",
		}),
		altsrc.NewStringFlag(cli.StringFlag{
			Name:  pprofInterval,
			Usage: "Interval to periodically collect pprof profiles (e.g. --pprofInterval=\"5m\")",
		}),
		altsrc.NewStringFlag(cli.StringFlag{
			Name: metricsLogDir,
			Usage: "directory for the metrics files (" + metrics.FileName + ", rotated periodically). " +
				"If unset, no metrics file is written. " + metricsNotice,
			Hidden: true,
		}),
		altsrc.NewStringFlag(cli.StringFlag{
			Name:  mvflags.DDLHandlingFlag,
			Value: string(verifier.DDLHandlingFailAll),
			Usage: "How to handle DDL events on the source. One of: " + strings.Join(
				lo.Map(
					verifier.DDLHandlingOpts,
					func(h verifier.DDLHandling, _ int) string {
						return string(h)
					},
				),
				", ",
			),
		}),
	}

	var metricsPipeline option.Option[*metrics.Pipeline]

	app := &cli.App{
		Name:    "migration-verifier",
		Usage:   "verify migration correctness",
		Version: fmt.Sprintf("%s, built at %s", buildvar.Revision, buildvar.BuildTime),
		Flags:   flags,
		Before: func(cCtx *cli.Context) error {
			confFile := cCtx.String(configFileFlag)

			if len(confFile) > 0 {
				readConfFunc := altsrc.InitInputSourceWithContext(flags, altsrc.NewYamlSourceFromFlagFunc(configFileFlag))
				return readConfFunc(cCtx)
			}

			return nil
		},
		Action: func(cCtx *cli.Context) error {
			verifier, err := handleArgs(ctx, cCtx)
			if err != nil {
				return err
			}

			if err := handleLogLevelArg(cCtx); err != nil {
				return err
			}

			if pipeline, has := setUpMetrics(cCtx, verifier).Get(); has {
				metricsPipeline = option.Some(pipeline)
				verifier.SetMetricsHandler(pipeline.Handler())
				verifier.SetMeterProvider(pipeline.MeterProvider())
			}

			if cCtx.Bool(checkOnly) {
				err := verifier.WritesOff(ctx)
				if err != nil {
					return errors.Wrap(err, "failed to set writes off")
				}

				return verifier.CheckDriver(ctx, nil)
			} else {
				return verifier.RunServer(ctx, cCtx.Bool(startFlag))
			}
		},
	}

	err := app.Run(os.Args)

	stopCatchingSignals()

	// log.Fatal and exitBySignal exit without running deferred calls, so this
	// can’t be a defer.
	if pipeline, has := metricsPipeline.Get(); has {
		shutDownMetrics(pipeline)
	}

	// After a signal, any error most likely comes from the shutdown, so it
	// isn’t fatal. The signal is the context’s cause: err itself rarely wraps
	// it.
	if sigErr, ok := stderrors.AsType[msignal.SignalError](context.Cause(ctx)); ok {
		log.Info().
			Stringer("signal", sigErr.Signal).
			AnErr("error", err).
			Msg("Shut down after signal.")

		exitBySignal(sigErr.Signal)
	}

	if err != nil {
		log.Fatal().Err(err).Stack().Msg("Fatal Error")
	}
}

// exitBySignal ends the process by sending it sig, which must have its default
// behavior again, so that the process exits as it would have without a signal
// handler. If the process survives, for example because sig is ignored, it
// exits with the status that shells report for a process that sig ended.
func exitBySignal(sig os.Signal) {
	if self, err := os.FindProcess(os.Getpid()); err == nil {
		_ = self.Signal(sig)
	}

	// Delivery is asynchronous, so give the signal a moment to end the
	// process.
	time.Sleep(time.Second)

	status := 1
	if num, isSyscallSignal := sig.(syscall.Signal); isSyscallSignal {
		status = 128 + int(num)
	}

	os.Exit(status)
}

// setUpMetrics creates the metrics pipeline. A failure is only logged:
// metrics never stop a verification.
func setUpMetrics(cCtx *cli.Context, v *verifier.Verifier) option.Option[*metrics.Pipeline] {
	settings := metrics.Settings{}
	if dir := cCtx.String(metricsLogDir); dir != "" {
		settings.FileDir = option.Some(dir)
	}

	pipeline, err := metrics.Setup(v.GetLogger(), settings)
	if err != nil {
		v.GetLogger().Warn().Err(err).Msg("Failed to set up metrics. Continuing without them.")
		return option.None[*metrics.Pipeline]()
	}

	return option.Some(pipeline)
}

func shutDownMetrics(pipeline *metrics.Pipeline) {
	ctx, cancel := contextplus.WithTimeoutCause(
		context.Background(),
		metricsShutdownTimeout,
		fmt.Errorf("timed out metrics shutdown"),
	)
	defer cancel()

	if err := pipeline.Shutdown(ctx); err != nil {
		log.Warn().Err(err).Msg("Failed to shut down metrics.")
	}
}

// getLogDir returns the log directory from --logDir or its deprecated name,
// --logPath. It fails if both are set to different values.
//
// This compares values rather than calling cCtx.IsSet, which misses values
// from the config file: urfave/cli v1 caches IsSet’s answers before altsrc
// applies the config file.
func getLogDir(cCtx *cli.Context) (string, error) {
	dir := cCtx.String(logPathAlias)
	if dir == "" {
		return cCtx.String(logDir), nil
	}

	if cCtx.String(logDir) != logDirDefault && cCtx.String(logDir) != dir {
		return "", fmt.Errorf(
			"--%s (%#q) conflicts with its deprecated name, --%s (%#q)",
			logDir,
			cCtx.String(logDir),
			logPathAlias,
			dir,
		)
	}

	return dir, nil
}

func handleLogLevelArg(cCtx *cli.Context) error {
	logLevelStr := cCtx.String(logLevelFlag)
	if !slices.Contains(logLevelStrs, logLevelStr) {
		return errors.Errorf("invalid %#q", logLevelFlag)
	}
	logLevel, err := zerolog.ParseLevel(logLevelStr)
	if err != nil {
		return errors.Wrapf(err, "parsing %#q", logLevelFlag)
	}

	zerolog.SetGlobalLevel(logLevel)

	return nil
}

// logConfig iterates through all set flags and logs them as a nested dictionary
func logConfig(c *cli.Context, logger *logger.Logger) {
	event := logger.Info()

	for _, flagName := range c.GlobalFlagNames() {
		val := c.String(flagName)

		if slices.Contains([]string{"srcURI", "dstURI", "metaURI"}, flagName) {
			opts := options.Client().ApplyURI(val)

			event.Strs(flagName+"-hosts", opts.Hosts)
		} else {
			event.Str(flagName, val)
		}
	}

	event.Msg("Active configuration")
}

func handleArgs(ctx context.Context, cCtx *cli.Context) (*verifier.Verifier, error) {
	verifierSettings := verifier.VerifierSettings{}
	if cCtx.Bool(ignoreReadConcernFlag) {
		verifierSettings.ReadConcernSetting = verifier.ReadConcernIgnore
	}

	missingStringArgs := lo.Filter(
		mslices.Of(srcURI, dstURI),
		func(setting string, _ int) bool {
			return cCtx.String(setting) == ""
		},
	)

	if len(missingStringArgs) > 0 {
		return nil, fmt.Errorf("missing required parameters: %#q", missingStringArgs)
	}

	logDirValue, err := getLogDir(cCtx)
	if err != nil {
		return nil, err
	}

	v := verifier.NewVerifier(verifierSettings, logDirValue)

	logger := v.GetLogger()

	logger.Info().
		Str("revision", buildvar.Revision).
		Str("buildTime", buildvar.BuildTime).
		Int("processID", os.Getpid()).
		Msg("migration-verifier started.")

	sysinfo.LogSystemInfo(
		ctx,
		slog.New(
			slogzerolog.Option{
				Logger: logger.Logger,
			}.NewZerologHandler(),
		),
	)

	logConfig(cCtx, logger)

	err = v.SetReadPreference(cCtx.String(readPreference))
	if err != nil {
		return nil, err
	}

	srcConnStr := cCtx.String(srcURI)
	_, srcConnStr, err = mongotools.MaybeAddDirectConnection(srcConnStr)
	if err != nil {
		return nil, errors.Wrap(err, "parsing source connection string")
	}
	err = v.SetSrcURI(ctx, srcConnStr)
	if err != nil {
		return nil, err
	}

	dstConnStr := cCtx.String(dstURI)
	_, dstConnStr, err = mongotools.MaybeAddDirectConnection(dstConnStr)
	if err != nil {
		return nil, errors.Wrap(err, "parsing destination connection string")
	}
	err = v.SetDstURI(ctx, dstConnStr)
	if err != nil {
		return nil, err
	}

	metaConnStr := cCtx.String(metaURI)

	if metaConnStr == "" {
		metaConnStr = dstConnStr

		v.GetLogger().Info().
			Msgf("Storing metadata on the destination cluster by default. Use a dedicated %#q instead if this underperforms.", metaURI)
	} else {
		_, metaConnStr, err = mongotools.MaybeAddDirectConnection(metaConnStr)
		if err != nil {
			return nil, errors.Wrap(err, "parsing metadata connection string")
		}
	}

	err = v.SetMetaURI(ctx, metaConnStr)
	if err != nil {
		return nil, err
	}

	v.SetServerPort(cCtx.Int(serverPort))

	if err := v.SetNumWorkers(cCtx.Int(numWorkers)); err != nil {
		return nil, fmt.Errorf("setting num workers: %w", err)
	}

	v.SetGenerationPauseDelay(time.Duration(cCtx.Int64(generationPauseDelay)) * time.Millisecond)
	v.SetWorkerSleepDelay(time.Duration(cCtx.Int64(workerSleepDelay)) * time.Millisecond)

	err = v.SetPprofInterval(cCtx.String(pprofInterval))
	if err != nil {
		return nil, err
	}

	partitionSizeMBVal := cCtx.Uint64(partitionSizeMB)

	if partitionSizeMBVal != 0 {
		if partitionSizeMBVal > math.MaxInt64 {
			return nil, fmt.Errorf("%q may not exceed %d", partitionSizeMB, math.MaxInt64)
		}
	}

	v.SetPartitionSizeMB(uint32(cmp.Or(partitionSizeMBVal, partitions.DefaultPartitionMiB)))

	v.SetStartClean(cCtx.Bool(startClean))

	if cCtx.Bool(verifyAll) {
		if len(cCtx.StringSlice(srcNamespace)) > 0 || len(cCtx.StringSlice(dstNamespace)) > 0 {
			return nil, errors.Errorf("Setting both verifyAll and explicit namespaces is not supported")
		}
		v.SetVerifyAll(true)
	} else {
		v.SetSrcNamespaces(expandCommaSeparators(cCtx.StringSlice(srcNamespace)))
		v.SetDstNamespaces(expandCommaSeparators(cCtx.StringSlice(dstNamespace)))
		v.SetNamespaceMap()
	}
	v.SetMetaDBName(cCtx.String(metaDBName))

	srcChangeReaderVal := cCtx.String(srcChangeReader)
	if !slices.Contains(verifier.ChangeReaderOpts, srcChangeReaderVal) {
		return nil, errors.Errorf("invalid %#q (%s); valid values are: %#q", srcChangeReader, srcChangeReaderVal, verifier.ChangeReaderOpts)
	}
	err = v.SetSrcChangeReaderMethod(srcChangeReaderVal)
	if err != nil {
		return nil, err
	}

	dstChangeReaderVal := cCtx.String(dstChangeReader)
	if !slices.Contains(verifier.ChangeReaderOpts, dstChangeReaderVal) {
		return nil, errors.Errorf("invalid %#q (%s); valid values are: %#q", dstChangeReader, dstChangeReaderVal, verifier.ChangeReaderOpts)
	}
	err = v.SetDstChangeReaderMethod(dstChangeReaderVal)
	if err != nil {
		return nil, err
	}

	ddlHandling := verifier.DDLHandling(cCtx.String(mvflags.DDLHandlingFlag))
	if !slices.Contains(verifier.DDLHandlingOpts, ddlHandling) {
		return nil, errors.Errorf("invalid %#q (%s); valid values are: %#q", mvflags.DDLHandlingFlag, ddlHandling, verifier.DDLHandlingOpts)
	}
	v.SetDDLHandling(ddlHandling)

	docCompareMethod := compare.Method(cCtx.String(docCompareMethod))
	err = v.SetDocCompareMethod(docCompareMethod)
	if err != nil {
		return nil, errors.Wrapf(err, "set doc compare method (%s)", docCompareMethod)
	}

	partitioningScheme := cCtx.String(partitioningScheme)
	if !slices.Contains(partitions.Schemes, partitioningScheme) {
		return nil, errors.Errorf("invalid partitioning scheme (%s); valid values are: %#q", partitioningScheme, partitions.Schemes)
	}
	v.SetPartitioningScheme(partitions.Scheme(partitioningScheme))

	v.SetFailureDisplaySize(cCtx.Int64(failureDisplaySize))

	if ignoreVals := cCtx.StringSlice(indexSpecIgnoreFlag); len(ignoreVals) > 0 {
		tolerances := lo.Map(
			expandCommaSeparators(ignoreVals),
			func(s string, _ int) api.IndexSpecTolerance {
				return api.IndexSpecTolerance(s)
			},
		)

		invalid := lo.Filter(tolerances, func(t api.IndexSpecTolerance, _ int) bool {
			return !slices.Contains(api.IndexMismatchTolerances(), t)
		})
		if len(invalid) > 0 {
			return nil, fmt.Errorf("invalid %s value(s): %q; valid values are: %q", indexSpecIgnoreFlag, invalid, api.IndexMismatchTolerances())
		}

		v.SetIndexSpecTolerances(tolerances)
	}

	return v, nil
}

func expandCommaSeparators(in []string) []string {
	ret := []string{}
	for _, ns := range in {
		multiples := strings.SplitSeq(ns, ",")
		for sub := range multiples {
			ret = append(ret, strings.Trim(sub, " \t"))
		}
	}
	return ret
}
