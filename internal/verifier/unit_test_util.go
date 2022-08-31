package verifier

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/suite"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type MongoInstance struct {
	port    string
	version string
	dir     string
}

type WithMongodsTestSuite struct {
	suite.Suite
	srcMongoInstance, dstMongoInstance, metaMongoInstance MongoInstance
	srcMongoClient, dstMongoClient, metaMongoClient       *mongo.Client
	initialDbNames                                        map[string]bool
}

func (suite *WithMongodsTestSuite) SetupSuite() {
	if testing.Short() {
		suite.T().Skip("Skipping mongod-requiring tests in short mode")
	}
	err := startTestMongods(suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)
	suite.Require().Nil(err)
	ctx := context.Background()
	clientOpts := options.Client().ApplyURI("mongodb://localhost:" + suite.srcMongoInstance.port).SetAppName("Verifier Test Suite")
	suite.srcMongoClient, err = mongo.Connect(ctx, clientOpts)
	suite.Require().Nil(err)
	clientOpts = options.Client().ApplyURI("mongodb://localhost:" + suite.dstMongoInstance.port).SetAppName("Verifier Test Suite")
	suite.dstMongoClient, err = mongo.Connect(ctx, clientOpts)
	suite.Require().Nil(err)
	clientOpts = options.Client().ApplyURI("mongodb://localhost:" + suite.metaMongoInstance.port).SetAppName("Verifier Test Suite")
	suite.metaMongoClient, err = mongo.Connect(ctx, clientOpts)
	suite.Require().Nil(err)
	suite.initialDbNames = map[string]bool{}
	for _, client := range []*mongo.Client{suite.srcMongoClient, suite.dstMongoClient, suite.metaMongoClient} {
		dbNames, err := client.ListDatabaseNames(ctx, bson.D{})
		suite.Require().Nil(err)
		for _, dbName := range dbNames {
			suite.initialDbNames[dbName] = true
		}
	}
}

func (suite *WithMongodsTestSuite) TearDownSuite() {
	stopTestMongods()
}

func (suite *WithMongodsTestSuite) TearDownTest() {
	ctx := context.Background()
	for _, client := range []*mongo.Client{suite.srcMongoClient, suite.dstMongoClient, suite.metaMongoClient} {
		dbNames, err := client.ListDatabaseNames(ctx, bson.D{})
		suite.Require().Nil(err)
		for _, dbName := range dbNames {
			if !suite.initialDbNames[dbName] {
				err = client.Database(dbName).Drop(ctx)
				suite.Require().Nil(err)
			}
		}
	}
}

var cachePath = filepath.Join("mongodb_exec")
var mongoDownloadMutex sync.Mutex

func stopTestMongods() {
	// ignore the error as this will fail if mongod is not already running
	_ = exec.Command("killall", "mongod").Run()
}

func startTestMongods(srcMongoInstance MongoInstance, dstMongoInstance MongoInstance, metaMongoInstance MongoInstance) error {
	stopTestMongods()
	srcMongod, err := getMongod(srcMongoInstance)
	if err != nil {
		return err
	}
	dstMongod, err := getMongod(dstMongoInstance)
	if err != nil {
		return err
	}
	metaMongod, err := getMongod(metaMongoInstance)
	if err != nil {
		return err
	}

	metaDir := filepath.Join(cachePath, metaMongoInstance.dir, "db")
	if err := os.RemoveAll(metaDir); err != nil {
		return err
	}
	if err := os.MkdirAll(metaDir, 0755); err != nil {
		return err
	}
	sourceDir := filepath.Join(cachePath, srcMongoInstance.dir, "db")
	if err := os.RemoveAll(sourceDir); err != nil {
		return err
	}
	if err := os.MkdirAll(sourceDir, 0755); err != nil {
		return err
	}
	destDir := filepath.Join(cachePath, dstMongoInstance.dir, "db")
	if err := os.RemoveAll(destDir); err != nil {
		return err
	}
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}

	logpath := func(target string) string {
		return filepath.Join(filepath.Dir(target), "log")
	}

	metaCmd := exec.Command(metaMongod, "--port", metaMongoInstance.port, "--dbpath", metaDir,
		"--logpath", logpath(metaDir))
	sourceCmd := exec.Command(srcMongod, "--port", srcMongoInstance.port, "--dbpath", sourceDir,
		"--logpath", logpath(sourceDir))
	destCmd := exec.Command(dstMongod, "--port", dstMongoInstance.port, "--dbpath", destDir,
		"--logpath", logpath(destDir))

	if err := metaCmd.Start(); err != nil {
		return err
	}
	if err := sourceCmd.Start(); err != nil {
		return err
	}
	if err := destCmd.Start(); err != nil {
		return err
	}
	return nil
}

func getMongod(mongoInstance MongoInstance) (string, error) {
	localMongoDistro := os.Getenv("MONGODB_DISTRO")
	if localMongoDistro == "" {
		err := errors.New(`there was no MONGODB_DISTRO specified. Please specify enviorment variable MONGODB_DISTRO.
For example, MONGODB_DISTRO=mongodb-linux-x86_64-rhel70 is the one used for rhel and MONGODB_DISTRO=mongodb-linux-x86_64-ubuntu1804 is the one used for ubuntu.
The link can be found at https://www.mongodb.com/try/download/community`)
		return "", err
	}
	localMongoDistro = localMongoDistro + "-" + mongoInstance.version

	mongoDBDir := filepath.Join(cachePath, localMongoDistro)
	mongod := filepath.Join(mongoDBDir, "bin", "mongod")
	mongoDownloadMutex.Lock()
	defer mongoDownloadMutex.Unlock()
	if _, err := os.Stat(mongoDBDir); os.IsNotExist(err) {
		url := fmt.Sprintf("https://fastdl.mongodb.org/linux/%s.tgz", localMongoDistro)
		fmt.Println("Downloading binary from " + url)
		r, err := curl(url)
		if err != nil {
			return "", err
		}
		defer r.Close()
		return mongod, untar(r, cachePath)
	}
	return mongod, nil
}

func curl(url string) (io.ReadCloser, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func untar(r io.Reader, dst string) error {
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)

	for {
		header, err := tr.Next()

		switch {

		// if no more files are found return
		case err == io.EOF:
			return nil

		// return any other error
		case err != nil:
			return err

		// if the header is nil, just skip it (not sure how this happens)
		case header == nil:
			continue
		}

		// the target location where the dir/file should be created
		target := filepath.Join(dst, header.Name)

		// check the file type
		switch header.Typeflag {

		// if its a dir and it doesn't exist create it
		case tar.TypeDir:
			if _, err := os.Stat(target); err != nil {
				if err := os.MkdirAll(target, 0755); err != nil {
					return err
				}
			}

		// if it's a file create it, but make sure to create the parent
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR, os.FileMode(header.Mode))
			if err != nil {
				return err
			}

			// copy over contents
			if _, err := io.Copy(f, tr); err != nil {
				return err
			}

			// manually close here after each file operation; defering would cause each file close
			// to wait until all operations have completed.
			f.Close()
		}
	}
}
