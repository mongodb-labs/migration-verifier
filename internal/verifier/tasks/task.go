package tasks

// Copyright (C) MongoDB, Inc. 2020-present.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

import (
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/option"
	"github.com/rs/zerolog"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type Type string
type Status string

const (
	// --------------------------------------------------
	// Task statuses:
	// --------------------------------------------------

	// This means the task is ready for work once a worker thread gets to it.
	Added Status = "added"

	// This means a worker thread is actively processing the task.
	Processing Status = "processing"

	// This means no mismatches were found. (Yay!)
	Completed Status = "completed"

	// This can mean a few different things. Generally it means that at least
	// one mismatch was found. (It does *not* mean that the verifier failed to
	// complete the verification task.)
	Failed Status = "failed"

	// This is used for collection verification. It means the task successfully
	// created the data tasks, but there were mismatches in the
	// metadata/indexes.
	MetadataMismatch Status = "mismatch"

	// --------------------------------------------------
	// Task types:
	// --------------------------------------------------

	// The “workhorse” task type: verify a partition of documents.
	VerifyDocuments Type = "verifyDocuments"

	// A verifyCollection task verifies collection metadata
	// and, in generation 0, inserts verify-documents tasks for _id ranges.
	VerifyCollection Type = "verifyCollection"

	// The primary task creates a verifyCollection task for each
	// namespace.
	Primary Type = "primary"
)

// Task stores source cluster info
type Task struct {
	PrimaryKey bson.ObjectID `bson:"_id"`
	Type       Type          `bson:"type"`
	Status     Status        `bson:"status"`
	Generation int           `bson:"generation"`

	// For recheck tasks, this stores the document IDs to check.
	Ids []bson.RawValue `bson:"_ids"`

	QueryFilter QueryFilter `bson:"query_filter" json:"query_filter"`

	// DocumentCount is set when the verifier is done with the task
	// (whether we found mismatches or not).
	SourceDocumentCount types.DocumentCount `bson:"source_documents_count"`

	// ByteCount is like DocumentCount: set when the verifier is done
	// with the task.
	SourceByteCount types.ByteCount `bson:"source_bytes_count"`

	// FirstMismatchTime correlates an index in Ids with the time when
	// this document was first seen to mismatch.
	FirstMismatchTime map[int32]bson.DateTime

	// SrcTimestamp records the optime of the latest source change event that
	// caused this (recheck) task to exist.
	SrcTimestamp option.Option[bson.Timestamp]

	// DstTimestamp is like SrcChangeOpTime but for the destination.
	DstTimestamp option.Option[bson.Timestamp]
}

func (t *Task) AugmentLogWithDetails(evt *zerolog.Event) {
	if len(t.Ids) > 0 {
		evt.Int("documentCount", len(t.Ids))
	} else {
		evt.
			Stringer("min", t.QueryFilter.Partition.Key.Lower).
			Stringer("max", t.QueryFilter.Partition.Upper)
	}
}

func (t *Task) IsRecheck() bool {
	return t.Generation > 0
}
