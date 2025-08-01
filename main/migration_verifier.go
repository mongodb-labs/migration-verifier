package main

import (
	"context"
	"fmt"
	"math"
	_ "net/http/pprof"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/10gen/migration-verifier/internal/verifier"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/rs/zerolog/pkgerrors"
	"github.com/samber/lo"
	"github.com/urfave/cli"
	"github.com/urfave/cli/altsrc"
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
	metaDBName            = "metaDBName"
	docCompareMethod      = "docCompareMethod"
	verifyAll             = "verifyAll"
	startClean            = "clean"
	readPreference        = "readPreference"
	partitionSizeMB       = "partitionSizeMB"
	recheckMaxSizeMB      = "recheckMaxSizeMB"
	checkOnly             = "checkOnly"
	debugFlag             = "debug"
	failureDisplaySize    = "failureDisplaySize"
	ignoreReadConcernFlag = "ignoreReadConcern"
	configFileFlag        = "configFile"
	pprofInterval         = "pprofInterval"

	buildVarDefaultStr = "Unknown; build with build.sh."
)

// These get set at build time, assuming use of build.sh.
var Revision = buildVarDefaultStr
var BuildTime = buildVarDefaultStr

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
			Value: "mongodb://localhost:27017",
			Usage: "source Host `URI` for migration verification",
		}),
		altsrc.NewStringFlag(cli.StringFlag{
			Name:  dstURI,
			Value: "mongodb://localhost:27018",
			Usage: "destination Host `URI` for migration verification",
		}),
		altsrc.NewStringFlag(cli.StringFlag{
			Name:  metaURI,
			Value: "mongodb://localhost:27019",
			Usage: "host `URI` for storing migration verification metadata",
		}),
		altsrc.NewIntFlag(cli.IntFlag{
			Name:  serverPort,
			Value: 27020,
			Usage: "`port` for the control web server",
		}),
		altsrc.NewStringFlag(cli.StringFlag{
			Name:  logPath,
			Value: "stdout",
			Usage: "logging file `path`",
		}),
		altsrc.NewIntFlag(cli.IntFlag{
			Name:  numWorkers,
			Value: 10,
			Usage: "`number` of worker threads to use for verification",
		}),
		altsrc.NewUintFlag(cli.UintFlag{
			Name:  recheckMaxSizeMB,
			Value: verifier.DefaultRecheckMaxSizeMB,
			Usage: "Maximum size of a recheck query. Reduce this to limit server memory usage after generation 0.",
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
		altsrc.NewStringSliceFlag(cli.StringSliceFlag{
			Name:  srcNamespace,
			Usage: "source `namespaces` to check",
		}),
		altsrc.NewStringSliceFlag(cli.StringSliceFlag{
			Name:  dstNamespace,
			Usage: "destination `namespaces` to check",
		}),
		altsrc.NewStringFlag(cli.StringFlag{
			Name:  metaDBName,
			Value: "migration_verification_metadata",
			Usage: "`name` of the database in which to store verification metadata",
		}),
		altsrc.NewStringFlag(cli.StringFlag{
			Name: docCompareMethod,
			Usage: "Method to compare documents. One of: " + strings.Join(
				lo.Map(
					verifier.DocCompareMethods,
					func(dcm verifier.DocCompareMethod, _ int) string {
						return string(dcm)
					},
				),
				", ",
			),
			Value: string(verifier.DocCompareDefault),
		}),
		altsrc.NewBoolFlag(cli.BoolFlag{
			Name:  verifyAll,
			Usage: "If set, verify all user namespaces",
		}),
		altsrc.NewBoolFlag(cli.BoolFlag{
			Name:  startClean,
			Usage: "If set, drop all previous verification metadata before starting",
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
		altsrc.NewBoolFlag(cli.BoolFlag{
			Name:  debugFlag,
			Usage: "Turn on debug logging",
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
			if cCtx.Bool(debugFlag) {
				zerolog.SetGlobalLevel(zerolog.DebugLevel)
			}
			if cCtx.Bool(checkOnly) {
				err := verifier.WritesOff(ctx)
				if err != nil {
					return errors.Wrap(err, "failed to set writes off")
				}

				return verifier.CheckDriver(ctx, nil)
			} else {
				return verifier.StartServer()
			}
		},
	}

	if err := app.Run(os.Args); err != nil {
		log.Fatal().Err(err).Stack().Msg("Fatal Error")
	}
}

func handleArgs(ctx context.Context, cCtx *cli.Context) (*verifier.Verifier, error) {
	verifierSettings := verifier.VerifierSettings{}
	if cCtx.Bool(ignoreReadConcernFlag) {
		verifierSettings.ReadConcernSetting = verifier.ReadConcernIgnore
	}

	logPath := cCtx.String(logPath)

	v := verifier.NewVerifier(verifierSettings, logPath)

	v.GetLogger().Info().
		Str("revision", Revision).
		Str("buildTime", BuildTime).
		Int("processID", os.Getpid()).
		Msg("migration-verifier started.")

	err := v.SetSrcURI(ctx, cCtx.String(srcURI))
	if err != nil {
		return nil, err
	}

	dstConnStr := cCtx.String(dstURI)
	err = v.SetDstURI(ctx, dstConnStr)
	if err != nil {
		return nil, err
	}

	metaConnStr := cCtx.String(metaURI)
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

		v.SetPartitionSizeMB(uint32(partitionSizeMB))
	}

	recheckMaxSizeMBVal := cCtx.Uint(recheckMaxSizeMB)
	if recheckMaxSizeMBVal != 0 {
		if recheckMaxSizeMBVal > verifier.MaxRecheckMaxSizeMB {
			return nil, fmt.Errorf("%#q may not exceed %d", recheckMaxSizeMB, verifier.MaxRecheckMaxSizeMB)
		}

		v.SetRecheckMaxSizeMB(recheckMaxSizeMBVal)
	}

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

	docCompareMethod := verifier.DocCompareMethod(cCtx.String(docCompareMethod))
	if !slices.Contains(verifier.DocCompareMethods, docCompareMethod) {
		return nil, errors.Errorf("invalid doc compare method (%s); valid value are: %v", docCompareMethod, verifier.DocCompareMethods)
	}
	v.SetDocCompareMethod(docCompareMethod)

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
