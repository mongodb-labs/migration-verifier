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
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

const replSet string = "rs0"

type MongoInstance struct {
	port    string
	version string
	process *os.Process
}

// SetPort sets the MongoInstance’s port so that clients can connect
// to it.
func (mi *MongoInstance) SetPort(port uint64) {
	mi.port = strconv.FormatUint(port, 10)
}

// SetProcess sets the MongoInstance’s process so that we can terminate
// the process once we’re done with it.
func (mi *MongoInstance) SetProcess(process *os.Process) {
	mi.process = process
}

type WithMongodsTestingSuite interface {
	suite.TestingSuite
	SetSrcInstance(MongoInstance)
	SetDstInstance(MongoInstance)
	SetMetaInstance(MongoInstance)
}

type WithMongodsTestSuite struct {
	suite.Suite
	srcMongoInstance, dstMongoInstance, metaMongoInstance MongoInstance
	srcMongoClient, dstMongoClient, metaMongoClient       *mongo.Client
	initialDbNames                                        map[string]bool
}

func (suite *WithMongodsTestSuite) SetSrcInstance(instance MongoInstance) {
	suite.srcMongoInstance = instance
}

func (suite *WithMongodsTestSuite) SetDstInstance(instance MongoInstance) {
	suite.dstMongoInstance = instance
}

func (suite *WithMongodsTestSuite) SetMetaInstance(instance MongoInstance) {
	suite.metaMongoInstance = instance
}

func (suite *WithMongodsTestSuite) SetupSuite() {
	if testing.Short() {
		suite.T().Skip("Skipping mongod-requiring tests in short mode")
	}
	err := startTestMongods(suite.T(), &suite.srcMongoInstance, &suite.dstMongoInstance, &suite.metaMongoInstance)
	suite.Require().NoError(err)
	ctx := context.Background()
	clientOpts := options.Client().ApplyURI("mongodb://localhost:" + suite.srcMongoInstance.port).SetAppName("Verifier Test Suite").SetWriteConcern(writeconcern.New(writeconcern.WMajority()))
	suite.srcMongoClient, err = mongo.Connect(ctx, clientOpts)
	suite.Require().NoError(err)
	clientOpts = options.Client().ApplyURI("mongodb://localhost:" + suite.dstMongoInstance.port).SetAppName("Verifier Test Suite").SetWriteConcern(writeconcern.New(writeconcern.WMajority()))
	suite.dstMongoClient, err = mongo.Connect(ctx, clientOpts)
	suite.Require().NoError(err)
	clientOpts = options.Client().ApplyURI("mongodb://localhost:" + suite.metaMongoInstance.port).SetAppName("Verifier Test Suite")
	suite.metaMongoClient, err = mongo.Connect(ctx, clientOpts)
	suite.startReplSet()
	suite.Require().NoError(err)
	suite.initialDbNames = map[string]bool{}
	for _, client := range []*mongo.Client{suite.srcMongoClient, suite.dstMongoClient, suite.metaMongoClient} {
		dbNames, err := client.ListDatabaseNames(ctx, bson.D{})
		suite.Require().NoError(err)
		for _, dbName := range dbNames {
			suite.initialDbNames[dbName] = true
		}
	}
}

func (suite *WithMongodsTestSuite) startReplSet() {
	ctx := context.Background()
	clientOpts := options.
		Client().
		ApplyURI("mongodb://localhost:" + suite.srcMongoInstance.port).
		SetDirect(true).
		SetAppName("Verifier Test Suite")
	directClient, err := mongo.Connect(ctx, clientOpts)
	suite.Require().NoError(err)
	command := bson.M{
		"replSetInitiate": bson.M{
			"_id": replSet,
			"members": bson.A{
				bson.M{"_id": 0, "host": "localhost:" + suite.srcMongoInstance.port},
			},
		},
	}
	err = directClient.Database("admin").RunCommand(ctx, command).Err()
	suite.Require().NoError(err)
}

func (suite *WithMongodsTestSuite) TearDownSuite() {
	suite.T().Log("Shutting down mongod instances …")

	instances := []*MongoInstance{
		&suite.srcMongoInstance,
		&suite.dstMongoInstance,
		&suite.metaMongoInstance,
	}

	theSignal := syscall.SIGTERM

	for _, instance := range instances {
		proc := instance.process

		if proc != nil {
			pid := instance.process.Pid
			suite.T().Logf("Sending SIGTERM to process %d", pid)
			err := instance.process.Signal(theSignal)
			if err != nil {
				suite.T().Logf("Failed to signal process %d: %v", pid, err)
			}
		}
	}
}

func (suite *WithMongodsTestSuite) TearDownTest() {
	ctx := context.Background()
	for _, client := range []*mongo.Client{suite.srcMongoClient, suite.dstMongoClient, suite.metaMongoClient} {
		dbNames, err := client.ListDatabaseNames(ctx, bson.D{})
		suite.Require().NoError(err)
		for _, dbName := range dbNames {
			if !suite.initialDbNames[dbName] {
				err = client.Database(dbName).Drop(ctx)
				suite.Require().NoError(err)
			}
		}
	}
}

var cachePath = filepath.Join("mongodb_exec")
var mongoDownloadMutex sync.Mutex

func startTestMongods(t *testing.T, srcMongoInstance *MongoInstance, dstMongoInstance *MongoInstance, metaMongoInstance *MongoInstance) error {

	// Ideally we’d start the mongods in parallel, but in development that
	// seemed to cause mongod to break on `--port 0`.
	start := time.Now()
	err := startOneMongod(t, srcMongoInstance, "--replSet", replSet)
	if err != nil {
		return err
	}

	err = startOneMongod(t, dstMongoInstance)
	if err != nil {
		return err
	}

	err = startOneMongod(t, metaMongoInstance)
	if err != nil {
		return err
	}

	t.Logf("Time elapsed creating mongod instances: %v", time.Since(start))

	return nil
}

func logpath(target string) string {
	return filepath.Join(target, "log")
}

// startOneMongod execs `path` with `extraArgs`. This mongod binds to
// “port 0”, which causes the OS to pick an arbitrary free port. It also
// creates a temporary directory for the mongod to do its work. Thus
// we avoid potential race conditions and interference between test
// runs & suites.
//
// The returns are the process, its listening TCP port, its dbpath,
// and whatever error may have happened.
func startOneMongod(t *testing.T, instance *MongoInstance, extraArgs ...string) error {

	// MongoDB 5.0+ writes its logs in line-delimited JSON;
	// older versions write free text.
	portRegexpJson := regexp.MustCompile(`"port":([1-9][0-9]*)`)
	portRegexp := regexp.MustCompile(`port\s([1-9][0-9]*)`)

	mongodPath, err := getMongod(*instance)
	if err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "*")
	if err != nil {
		return err
	}

	lpath := logpath(dir)

	cmdargs := []string{
		"--port", "0",
		"--dbpath", dir,
		"--logpath", lpath,
	}

	cmdargs = append(cmdargs, extraArgs...)

	t.Logf("Starting mongod: %s %v", mongodPath, cmdargs)

	cmd := exec.Command(mongodPath, cmdargs...)

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return err
	}

	instance.SetProcess(cmd.Process)

	pid := cmd.Process.Pid

	var finished atomic.Bool

	go func() {
		err := cmd.Wait()
		finished.Store(true)
		if err == nil {
			t.Logf("mongod process %d ended successfully", pid)
		} else {
			t.Logf("mongod process %d: %v", pid, err)
		}

		err = os.RemoveAll(dir)
		if err != nil {
			t.Logf("Failed to remove %s: %v", dir, err)
		}
	}()

	duration := time.Minute * 5
	endAt := time.Now().Add(duration)

	for time.Now().Before(endAt) {
		if finished.Load() {
			return fmt.Errorf("mongod process %d ended without storing its listening port in %s", pid, lpath)
		}

		content, err := os.ReadFile(lpath)

		if err == nil {
			match := portRegexpJson.FindSubmatch(content)
			if match == nil {
				match = portRegexp.FindSubmatch(content)
			}

			if match != nil {
				port, _ := strconv.ParseUint(string(match[1]), 10, 16)
				instance.SetPort(port)
				return nil
			}
		} else if os.IsNotExist(err) {
			// The log file isn’t created (yet?); loop again.
		} else {
			return fmt.Errorf("Unexpected error while reading logfile %s: %v", lpath, err)
		}

		time.Sleep(time.Millisecond * 100)
	}

	return fmt.Errorf("Timed out (%v) waiting to find mongod process %d’s listening port in %s", duration, pid, lpath)
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

	// Ensure that the HTTP response was a 2xx.
	if resp.StatusCode/200 != 1 {
		return nil, fmt.Errorf("HTTP %s (%s)", resp.Status, url)
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
