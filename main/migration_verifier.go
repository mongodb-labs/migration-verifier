package main

import (
	"context"
	"fmt"
	"math"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strings"
	"time"

	"github.com/10gen/migration-verifier/internal/verifier"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/rs/zerolog/pkgerrors"
	"github.com/urfave/cli"
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
	ignoreFieldOrder      = "ignoreFieldOrder"
	verifyAll             = "verifyAll"
	startClean            = "clean"
	readPreference        = "readPreference"
	partitionSizeMB       = "partitionSizeMB"
	checkOnly             = "checkOnly"
	debugFlag             = "debug"
	failureDisplaySize    = "failureDisplaySize"
	ignoreReadConcernFlag = "ignoreReadConcern"
)

func main() {
	go func() {
		fmt.Println(http.ListenAndServe("localhost:6060", nil))
	}()
	zerolog.ErrorStackMarshaler = pkgerrors.MarshalStack

	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	ctx := context.TODO()

	flags := []cli.Flag{
		&cli.StringFlag{
			Name:  srcURI,
			Value: "mongodb://localhost:27017",
			Usage: "source Host `URI` for migration verification",
		},
		&cli.StringFlag{
			Name:  dstURI,
			Value: "mongodb://localhost:27018",
			Usage: "destination Host `URI` for migration verification",
		},
		&cli.StringFlag{
			Name:  metaURI,
			Value: "mongodb://localhost:27019",
			Usage: "host `URI` for storing migration verification metadata",
		},
		&cli.IntFlag{
			Name:  serverPort,
			Value: 27020,
			Usage: "`port` for the control web server",
		},
		&cli.StringFlag{
			Name:  logPath,
			Value: "stdout",
			Usage: "logging file `path`",
		},
		&cli.IntFlag{
			Name:  numWorkers,
			Value: 10,
			Usage: "`number` of worker threads to use for verification",
		},
		&cli.Int64Flag{
			Name:  generationPauseDelay,
			Value: 1_000,
			Usage: "`milliseconds` to wait between generations of rechecking, allowing for more time to turn off writes",
		},
		&cli.Int64Flag{
			Name:  workerSleepDelay,
			Value: 1_000,
			Usage: "`milliseconds` workers sleep while waiting for work",
		},
		&cli.StringSliceFlag{
			Name:  srcNamespace,
			Usage: "source `namespaces` to check",
		},
		&cli.StringSliceFlag{
			Name:  dstNamespace,
			Usage: "destination `namespaces` to check",
		},
		&cli.StringFlag{
			Name:  metaDBName,
			Value: "migration_verification_metadata",
			Usage: "`name` of the database in which to store verification metadata",
		},
		&cli.BoolFlag{
			Name:  ignoreFieldOrder,
			Usage: "Whether or not field order is ignored in documents",
		},
		&cli.BoolFlag{
			Name:  verifyAll,
			Usage: "If set, verify all user namespaces",
		},
		&cli.BoolFlag{
			Name:  startClean,
			Usage: "If set, drop all previous verification metadata before starting",
		},
		&cli.StringFlag{
			Name:  readPreference,
			Value: "nearest",
			Usage: "Read preference for reading data from clusters. " +
				"May be 'primary', 'secondary', 'primaryPreferred', 'secondaryPreferred', or 'nearest'",
		},
		&cli.Uint64Flag{
			Name:  partitionSizeMB,
			Value: 0,
			Usage: "`Megabytes` to use for a partition.  Change only for debugging. 0 means use partitioner default.",
		},
		&cli.BoolFlag{
			Name:  debugFlag,
			Usage: "Turn on debug logging",
		},
		&cli.BoolFlag{
			Name:  checkOnly,
			Usage: "Do not run the webserver or recheck, just run the check (for debugging)",
		},
		&cli.Int64Flag{
			Name:  failureDisplaySize,
			Value: verifier.DefaultFailureDisplaySize,
			Usage: "Number of failures to display. Will display all failures if the number doesn’t exceed this limit by 25%",
		},
		&cli.BoolFlag{
			Name:  ignoreReadConcernFlag,
			Usage: "Use connection-default read concerns rather than setting majority read concern. This option may degrade consistency, so only enable it if majority read concern (the default) doesn’t work.",
		},
	}
	app := &cli.App{
		Name:  "migration-verifier",
		Usage: "verify migration correctness",
		Flags: flags,
		Action: func(cCtx *cli.Context) error {
			verifier, err := handleArgs(ctx, cCtx)
			if err != nil {
				return err
			}
			if cCtx.Bool(debugFlag) {
				zerolog.SetGlobalLevel(zerolog.DebugLevel)
			}
			if cCtx.Bool(checkOnly) {
				verifier.WritesOff(ctx)
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

	v := verifier.NewVerifier(verifierSettings)
	err := v.SetSrcURI(ctx, cCtx.String(srcURI))
	if err != nil {
		return nil, err
	}
	err = v.SetDstURI(ctx, cCtx.String(dstURI))
	if err != nil {
		return nil, err
	}
	err = v.SetMetaURI(ctx, cCtx.String(metaURI))
	if err != nil {
		return nil, err
	}
	v.SetServerPort(cCtx.Int(serverPort))
	v.SetNumWorkers(cCtx.Int(numWorkers))
	v.SetGenerationPauseDelayMillis(time.Duration(cCtx.Int64(generationPauseDelay)))
	v.SetWorkerSleepDelayMillis(time.Duration(cCtx.Int64(workerSleepDelay)))

	partitionSizeMB := cCtx.Uint64(partitionSizeMB)
	if partitionSizeMB != 0 {
		if partitionSizeMB > math.MaxInt64 {
			return nil, fmt.Errorf("%q may not exceed %d", partitionSizeMB, math.MaxInt64)
		}

		v.SetPartitionSizeMB(uint32(partitionSizeMB))
	}

	v.SetStartClean(cCtx.Bool(startClean))
	logPath := cCtx.String(logPath)
	v.SetLogger(logPath)
	if err != nil {
		return nil, err
	}
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
	v.SetIgnoreBSONFieldOrder(cCtx.Bool(ignoreFieldOrder))
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
