package verifier

// Copyright (C) MongoDB, Inc. 2020-present.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

import (
	"context"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func BenchmarkGeneric(t *testing.B) {
	go func() {
		fmt.Println(http.ListenAndServe("localhost:6060", nil))
	}()
	metaUri := os.Getenv("META_URI") // "mongodb://localhost:"+metaMongoInstance.port
	srcUri := os.Getenv("SRC_URI")
	dstUri := os.Getenv("DST_URI")
	namespace := os.Getenv("NAMESPACES") // "keyhole.dealers,keyhole.dealers"
	workers := os.Getenv("WORKERS")
	metaDBName := os.Getenv("META_DB_NAME")
	numWorkers := 0

	if namespace == "" {
		t.Fatal("Namespace cannot be empty. Specify one or more comma delimated namespaces")
	}

	namespaces := strings.Split(namespace, ",")

	if workers == "" {
		numWorkers = runtime.NumCPU()
	} else {
		workers, err := strconv.Atoi(workers)
		if err != nil {
			t.Fatal(err)
		}
		numWorkers = workers
	}

	if metaDBName == "" {
		metaDBName = "VERIFIER_META"
	}

	fmt.Printf("Running with %d workers. Specify WORKERS= to change\n", numWorkers)
	fmt.Printf("Running with %s as the meta db name. Specify META_DB_NAME= to change\n", metaDBName)
	// fmt.Printf("Running with %s as the namespace. Specify META_DB_NAME= to change\n", metaDBName)

	verifier := NewVerifier(VerifierSettings{}, "stderr")
	verifier.SetNumWorkers(numWorkers)
	verifier.SetGenerationPauseDelayMillis(0)
	verifier.SetWorkerSleepDelayMillis(0)
	fmt.Printf("meta uri %s\n", metaUri)
	err := verifier.SetMetaURI(context.Background(), metaUri)
	if err != nil {
		t.Fatal(err)
	}
	err = verifier.SetSrcURI(context.Background(), srcUri)
	if err != nil {
		t.Fatal(err)
	}
	err = verifier.SetDstURI(context.Background(), dstUri)
	if err != nil {
		t.Fatal(err)
	}
	verifier.SetMetaDBName(metaDBName)
	err = verifier.verificationTaskCollection().Drop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = verifier.verificationDatabase().Collection(recheckQueue).Drop(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	println("Starting tasks")
	for _, namespace := range namespaces {
		fmt.Printf("Starting task with '%s' namespace\n", namespace)
		qfilter := QueryFilter{Namespace: namespace}
		task := VerificationTask{QueryFilter: qfilter}
		// TODO: is this safe?
		mismatchedIds, docsCount, bytesCount, err := verifier.FetchAndCompareDocuments(context.Background(), &task)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Printf("In namespace %s found %d mismatched docs out of %d total (%d bytes)", namespace, len(mismatchedIds), docsCount, bytesCount)
	}
}
