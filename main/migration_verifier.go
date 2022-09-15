package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"time"

	"github.com/10gen/migration-verifier/internal/verifier"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/rs/zerolog/pkgerrors"
	"github.com/urfave/cli"
)

const (
	srcURI               = "srcURI"
	dstURI               = "dstURI"
	metaURI              = "metaURI"
	numWorkers           = "numWorkers"
	comparisonRetryDelay = "comparisonRetryDelay"
	workerSleepDelay     = "workerSleepDelay"
	serverPort           = "serverPort"
	logPath              = "logPath"
	srcNamespace         = "srcNamespace"
	dstNamespace         = "dstNamespace"
	metaDBName           = "metaDBName"
	ignoreFieldOrder     = "ignoreFieldOrder"
	verifyAll            = "verifyAll"
	startClean           = "clean"
	readPreference       = "readPreference"
	partitionSizeMB      = "partitionSizeMB"
	checkOnly            = "checkOnly"
	debugFlag            = "debug"
)

func main() {
	go func() {
		fmt.Println(http.ListenAndServe("localhost:6060", nil))
	}()
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
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
			Value: "stderr",
			Usage: "logging file `path`",
		},
		&cli.IntFlag{
			Name:  numWorkers,
			Value: 10,
			Usage: "`number` of worker threads to use for verification",
		},
		&cli.Int64Flag{
			Name:  comparisonRetryDelay,
			Value: 1_000,
			Usage: "`milliseconds` to wait between retries on a comparisonRetryDelay",
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
		&cli.Int64Flag{
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
	}
	app := &cli.App{
		Name:  "migration-verifier",
		Usage: "verify migration correctness",
		Flags: flags,
		Action: func(cCtx *cli.Context) error {
			verifier, logFile, logWriter, err := handleArgs(ctx, cCtx)
			if logFile != nil {
				defer func() {
					logWriter.Flush()
					logFile.Close()
				}()
			}
			if err != nil {
				return err
			}
			if cCtx.Bool(debugFlag) {
				zerolog.SetGlobalLevel(zerolog.DebugLevel)
			}
			if cCtx.Bool(checkOnly) {
				return verifier.Check(ctx)
			} else {
				return verifier.StartServer()
			}
		},
	}

	if err := app.Run(os.Args); err != nil {
		log.Fatal().Err(err).Stack().Msg("Fatal Error")
	}
}

func handleArgs(ctx context.Context, cCtx *cli.Context) (*verifier.Verifier, *os.File, *bufio.Writer, error) {
	v := verifier.NewVerifier()
	err := v.SetSrcURI(ctx, cCtx.String(srcURI))
	if err != nil {
		return nil, nil, nil, err
	}
	err = v.SetDstURI(ctx, cCtx.String(dstURI))
	if err != nil {
		return nil, nil, nil, err
	}
	err = v.SetMetaURI(ctx, cCtx.String(metaURI))
	if err != nil {
		return nil, nil, nil, err
	}
	v.SetServerPort(cCtx.Int(serverPort))
	v.SetNumWorkers(cCtx.Int(numWorkers))
	v.SetComparisonRetryDelayMillis(time.Duration(cCtx.Int64(comparisonRetryDelay)))
	v.SetWorkerSleepDelayMillis(time.Duration(cCtx.Int64(workerSleepDelay)))
	v.SetPartitionSizeMB(cCtx.Int64(partitionSizeMB))
	v.SetStartClean(cCtx.Bool(startClean))
	logPath := cCtx.String(logPath)
	file, writer, err := v.SetLogger(logPath)
	if err != nil {
		return nil, nil, nil, err
	}
	if cCtx.Bool(verifyAll) {
		if len(cCtx.StringSlice(srcNamespace)) > 0 || len(cCtx.StringSlice(dstNamespace)) > 0 {
			return nil, nil, nil, errors.Errorf("Setting both verifyAll and explicit namespaces is not supported")
		}
		v.SetVerifyAll(true)
	} else {
		v.SetSrcNamespaces(cCtx.StringSlice(srcNamespace))
		v.SetDstNamespaces(cCtx.StringSlice(dstNamespace))
	}
	v.SetMetaDBName(cCtx.String(metaDBName))
	v.SetIgnoreBSONFieldOrder(cCtx.Bool(ignoreFieldOrder))
	err = v.SetReadPreference(cCtx.String(readPreference))
	if err != nil {
		return nil, nil, nil, err
	}
	return v, file, writer, nil
}
