package main

import (
	"bufio"
	"context"
	"os"
	"path"
	"time"

	"github.com/10gen/migration-verifier/internal/verifier"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/urfave/cli"
)

const (
	srcURI               = "srcURI"
	dstURI               = "dstURI"
	metaURI              = "metaURI"
	numWorkers           = "numWorkers"
	comparisonRetryDelay = "comparisonRetryDelay"
	workerSleepDelay     = "workerSleepDelay"
	logPath              = "logPath"
	srcNamespaces        = "srcNamespaces"
	dstNamespaces        = "dstNamespaces"
	metaDBName           = "metaDBName"
)

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
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
			Name:  srcNamespaces,
			Usage: "source `namespaces` to check",
		},
		&cli.StringSliceFlag{
			Name:  dstNamespaces,
			Usage: "destination `namespaces` to check",
		},
		&cli.StringFlag{
			Name:  metaDBName,
			Value: "migration_verification_metadata",
			Usage: "`name` of the database in which to store verification metadata",
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
			return verifier.Verify()
		},
	}

	if err := app.Run(os.Args); err != nil {
		log.Fatal().Err(err).Msg("Migration Verifier failed")
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
	v.SetNumWorkers(cCtx.Int(numWorkers))
	v.SetComparisonRetryDelayMillis(time.Duration(cCtx.Int64(comparisonRetryDelay)))
	v.SetWorkerSleepDelayMillis(time.Duration(cCtx.Int64(workerSleepDelay)))
	logPath := cCtx.String(logPath)
	var file *os.File
	var writer *bufio.Writer = nil
	if logPath == "stderr" {
		l := zerolog.New(os.Stderr).With().Timestamp().Logger()
		v.SetLogger(&l)
	} else {
		if _, err := os.Stat(logPath); os.IsNotExist(err) {
			mkdirErr := os.MkdirAll(path.Dir(logPath), 0770)
			if mkdirErr != nil {
				return nil, nil, nil, mkdirErr
			}
		}
		file, err = os.Create(logPath)
		if err != nil {
			return nil, nil, nil, err
		}
		writer = bufio.NewWriter(file)
		l := zerolog.New(writer).With().Timestamp().Logger()
		v.SetLogger(&l)
	}
	v.SetSrcNamespaces(cCtx.StringSlice(srcNamespaces))
	v.SetDstNamespaces(cCtx.StringSlice(dstNamespaces))
	v.SetMetaDBName(cCtx.String(metaDBName))
	return v, file, writer, nil
}
