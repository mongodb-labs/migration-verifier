package verifier

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	mongoDistro               = "mongodb-linux-x86_64-rhel70-6.0.1"
	metaPort                  = "27001"
	sourcePort                = "27002"
	destPort                  = "27003"
	defaultWaitForMongodsTime = 5 * time.Second
)

var cachePath = filepath.Join("..", "..", "evergreen")

func stopTestMongods() {
	// ignore the error as this will fail if mongod is not already running
	exec.Command("killall", "mongod").Run()
}

func startTestMongods() error {
	stopTestMongods()
	mongod, err := getMongod()
	if err != nil {
		return err
	}

	metaDir := filepath.Join(cachePath, "meta", "db")
	if err := os.MkdirAll(metaDir, 0755); err != nil {
		return err
	}
	sourceDir := filepath.Join(cachePath, "source", "db")
	if err := os.MkdirAll(sourceDir, 0755); err != nil {
		return err
	}
	destDir := filepath.Join(cachePath, "dest", "db")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}

	logpath := func(target string) string {
		return filepath.Join(filepath.Dir(target), "log")
	}

	metaCmd := exec.Command(mongod, "--port", metaPort, "--dbpath", metaDir, "--logpath", logpath(metaDir))
	sourceCmd := exec.Command(mongod, "--port", sourcePort, "--dbpath", sourceDir, "--logpath", logpath(sourceDir))
	destCmd := exec.Command(mongod, "--port", destPort, "--dbpath", destDir, "--logpath", logpath(destDir))

	if err := metaCmd.Start(); err != nil {
		return err
	}
	if err := sourceCmd.Start(); err != nil {
		return err
	}
	if err := destCmd.Start(); err != nil {
		return err
	}

	time.Sleep(defaultWaitForMongodsTime)

	return nil
}

func getMongod() (string, error) {
	localMongoDistro := os.Getenv("MONGODB_DISTRO")
	if localMongoDistro == "" {
		localMongoDistro = mongoDistro
	}

	mongoDBDir := filepath.Join(cachePath, localMongoDistro)
	mongod := filepath.Join(mongoDBDir, "bin", "mongod")
	if _, err := os.Stat(mongoDBDir); os.IsNotExist(err) {
		url := fmt.Sprintf("https://fastdl.mongodb.org/linux/%s.tgz", localMongoDistro)
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
