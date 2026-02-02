package main

import (
	"cmp"
	"context"
	"fmt"
	"math"
	_ "net/http/pprof"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/verifier"
	"github.com/10gen/migration-verifier/internal/verifier/compare"
	"github.com/10gen/migration-verifier/mmongo"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/rs/zerolog/pkgerrors"
	"github.com/samber/lo"
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
	logPath               = "logPath"
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

	buildVarDefaultStr = "Unknown; build with build.sh."
)

// These get set at build time, assuming use of build.sh.
var Revision = buildVarDefaultStr
var BuildTime = buildVarDefaultStr

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
	ctx := context.TODO()

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
			Value: "mongodb://localhost",
			Usage: "connection string to replset that stores verifier metadata",
		}),
		altsrc.NewIntFlag(cli.IntFlag{
			Name:  serverPort,
			Value: 27020,
			Usage: "`port` for the control web server (0 assigns a random port)",
		}),
		altsrc.NewStringFlag(cli.StringFlag{
			Name:  logPath,
			Value: "stdout",
			Usage: "logging file `path`",
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
			Value: "migration_verification_metadata",
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
		altsrc.NewBoolFlag(cli.BoolFlag{
			Name:  ignoreReadConcernFlag,
			Usage: "Use connection-default read concerns rather than setting majority read concern. This option may degrade consistency, so only enable it if majority read concern (the default) doesn’t work.",
		}),
		altsrc.NewStringFlag(cli.StringFlag{
			Name:  pprofInterval,
			Usage: "Interval to periodically collect pprof profiles (e.g. --pprofInterval=\"5m\")",
		}),
	}

	app := &cli.App{
		Name:    "migration-verifier",
		Usage:   "verify migration correctness",
		Version: fmt.Sprintf("%s, built at %s", Revision, BuildTime),
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

			if cCtx.Bool(checkOnly) {
				err := verifier.WritesOff(ctx)
				if err != nil {
					return errors.Wrap(err, "failed to set writes off")
				}

				return verifier.CheckDriver(ctx, nil)
			} else {
				if cCtx.Bool(startFlag) {
					verifier.Check(ctx, nil)
				}

				return verifier.StartServer()
			}
		},
	}

	if err := app.Run(os.Args); err != nil {
		log.Fatal().Err(err).Stack().Msg("Fatal Error")
	}
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

	logPath := cCtx.String(logPath)

	v := verifier.NewVerifier(verifierSettings, logPath)

	logger := v.GetLogger()

	logger.Info().
		Str("revision", Revision).
		Str("buildTime", BuildTime).
		Int("processID", os.Getpid()).
		Msg("migration-verifier started.")

	logConfig(cCtx, logger)

	srcConnStr := cCtx.String(srcURI)
	_, srcConnStr, err := mmongo.MaybeAddDirectConnection(srcConnStr)
	if err != nil {
		return nil, errors.Wrap(err, "parsing source connection string")
	}
	err = v.SetSrcURI(ctx, srcConnStr)
	if err != nil {
		return nil, err
	}

	dstConnStr := cCtx.String(dstURI)
	_, dstConnStr, err = mmongo.MaybeAddDirectConnection(dstConnStr)
	if err != nil {
		return nil, errors.Wrap(err, "parsing destination connection string")
	}
	err = v.SetDstURI(ctx, dstConnStr)
	if err != nil {
		return nil, err
	}

	metaConnStr := cCtx.String(metaURI)
	_, metaConnStr, err = mmongo.MaybeAddDirectConnection(metaConnStr)
	if err != nil {
		return nil, errors.Wrap(err, "parsing metadata connection string")
	}
	err = v.SetMetaURI(ctx, metaConnStr)
	if err != nil {
		return nil, err
	}

	if dstConnStr == metaConnStr {
		v.GetLogger().Warn().
			Msg("Storing migration-verifier’s metadata on the destination can significantly impede performance. Use a dedicated cluster for the metadata if you can.")
	}

	v.SetServerPort(cCtx.Int(serverPort))
	v.SetNumWorkers(cCtx.Int(numWorkers))
	v.SetGenerationPauseDelay(time.Duration(cCtx.Int64(generationPauseDelay)) * time.Millisecond)
	v.SetWorkerSleepDelay(time.Duration(cCtx.Int64(workerSleepDelay)) * time.Millisecond)

	err = v.SetPprofInterval(cCtx.String(pprofInterval))
	if err != nil {
		return nil, err
	}

	partitionSizeMB := cCtx.Uint64(partitionSizeMB)

	if partitionSizeMB != 0 {
		if partitionSizeMB > math.MaxInt64 {
			return nil, fmt.Errorf("%q may not exceed %d", partitionSizeMB, math.MaxInt64)
		}

	}

	v.SetPartitionSizeMB(uint32(cmp.Or(partitionSizeMB, partitions.DefaultPartitionMiB)))

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

	docCompareMethod := compare.Method(cCtx.String(docCompareMethod))
	if !slices.Contains(compare.Methods, docCompareMethod) {
		return nil, errors.Errorf("invalid doc compare method (%s); valid values are: %#q", docCompareMethod, compare.Methods)
	}
	v.SetDocCompareMethod(docCompareMethod)

	partitioningScheme := cCtx.String(partitioningScheme)
	if !slices.Contains(partitions.Schemes, partitioningScheme) {
		return nil, errors.Errorf("invalid partitioning scheme (%s); valid values are: %#q", partitioningScheme, partitions.Schemes)
	}
	v.SetPartitioningScheme(partitions.Scheme(partitioningScheme))

	err = v.SetReadPreference(cCtx.String(readPreference))
	if err != nil {
		return nil, err
	}
	v.SetFailureDisplaySize(cCtx.Int64(failureDisplaySize))
	return v, nil
}

func expandCommaSeparators(in []string) []string {
	ret := []string{}
	for _, ns := range in {
		multiples := strings.Split(ns, ",")
		for _, sub := range multiples {
			ret = append(ret, strings.Trim(sub, " \t"))
		}
	}
	return ret
}
