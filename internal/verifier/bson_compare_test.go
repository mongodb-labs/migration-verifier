package verifier

// Copyright (C) MongoDB, Inc. 2020-present.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

import (
	"testing"

	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"go.mongodb.org/mongo-driver/bson"
)

func compareDocuments(srcDoc, dstDoc bson.D) (*MismatchDetails, error) {
	var srcRaw, dstRaw bson.Raw
	var err error
	srcRaw, err = bson.Marshal(srcDoc)
	if err != nil {
		log.Fatal().Err(err).Msgf("Could not marshal test src document (programming error): %s", srcDoc)
	}
	dstRaw, err = bson.Marshal(dstDoc)
	if err != nil {
		log.Fatal().Err(err).Msgf("Could not marshal test dst document (programming error) %s", dstDoc)
	}
	return BsonUnorderedCompareRawDocumentWithDetails(srcRaw, dstRaw)
}

func TestBSONUnorderedCompare(t *testing.T) {
	// Identical documents
	srcDoc := bson.D{
		{"_id", "a"},
		{"a", 1},
		{"b", 2}}

	result, err := compareDocuments(srcDoc, srcDoc)
	assert.NoError(t, err)
	assert.Nil(t, result)

	// Missing fields on destination
	dstDoc := bson.D{
		{"_id", "a"}}
	result, err = compareDocuments(srcDoc, dstDoc)
	assert.NoError(t, err)
	if assert.NotNil(t, result) {
		assert.Empty(t, result.missingFieldOnSrc)
		assert.ElementsMatch(t, result.missingFieldOnDst, []string{"a", "b"})
		assert.Empty(t, result.fieldContentsDiffer)
	}

	// Missing fields on src
	dstDoc = bson.D{
		{"_id", "a"},
		{"a", 1},
		{"aa", 11},
		{"b", 2},
		{"c", 3}}
	result, err = compareDocuments(srcDoc, dstDoc)
	assert.NoError(t, err)
	if assert.NotNil(t, result) {
		assert.ElementsMatch(t, result.missingFieldOnSrc, []string{"aa", "c"})
		assert.Empty(t, result.missingFieldOnDst)
		assert.Empty(t, result.fieldContentsDiffer)
	}

	// Top level order is changed
	dstDoc = bson.D{
		{"_id", "a"},
		{"b", 2},
		{"a", 1}}
	result, err = compareDocuments(srcDoc, dstDoc)
	assert.NoError(t, err)
	assert.Nil(t, result)

	// Sub-doc order is changed
	srcDoc = bson.D{
		{"_id", "a"},
		{"a", 1},
		{"b", 2},
		{"c", bson.D{{"d", 1}, {"e", 2}, {"f", 3}}}}
	dstDoc = bson.D{
		{"_id", "a"},
		{"a", 1},
		{"b", 2},
		{"c", bson.D{{"e", 2}, {"f", 3}, {"d", 1}}}}
	result, err = compareDocuments(srcDoc, dstDoc)
	assert.NoError(t, err)
	assert.Nil(t, result)

	// Different types
	srcDoc = bson.D{
		{"_id", "a"},
		{"a", 1},
		{"b", 2},
		{"c", bson.D{{"d", 1}, {"e", 2}, {"f", 3}}}}
	dstDoc = bson.D{
		{"_id", "a"},
		{"a", 1},
		{"b", "2"},
		{"c", "cvalue"}}
	result, err = compareDocuments(srcDoc, dstDoc)
	assert.NoError(t, err)
	if assert.NotNil(t, result) {
		assert.Empty(t, result.missingFieldOnSrc)
		assert.Empty(t, result.missingFieldOnDst)
		assert.ElementsMatch(t, result.fieldContentsDiffer, []string{"b", "c"})
	}

	// Different values
	srcDoc = bson.D{
		{"_id", "a"},
		{"a", 1},
		{"b", 2},
		{"c", bson.D{{"d", 1}, {"e", 2}, {"f", 3}}}}
	dstDoc = bson.D{
		{"_id", "a"},
		{"a", 1},
		{"b", 3},
		{"c", bson.D{{"d", 1}, {"e", 2}, {"f", 4}}}}
	result, err = compareDocuments(srcDoc, dstDoc)
	assert.NoError(t, err)
	if assert.NotNil(t, result) {
		assert.Empty(t, result.missingFieldOnSrc)
		assert.Empty(t, result.missingFieldOnDst)
		assert.ElementsMatch(t, result.fieldContentsDiffer, []string{"b", "c"})
	}

	// Multiple mismtaches of different sorts
	srcDoc = bson.D{
		{"_id", "a"},
		{"a", 1},
		{"b", 2},
		{"bb", 3},
		{"c", bson.D{{"d", 1}, {"e", 2}, {"f", 3}}}}
	dstDoc = bson.D{
		{"_id", "a"},
		{"a", 1},
		{"b", 2},
		{"e", 99},
		{"c", bson.D{{"d", 1}, {"e", 2}, {"f", 4}}}}
	result, err = compareDocuments(srcDoc, dstDoc)
	assert.NoError(t, err)
	if assert.NotNil(t, result) {
		assert.ElementsMatch(t, result.missingFieldOnSrc, []string{"e"})
		assert.ElementsMatch(t, result.missingFieldOnDst, []string{"bb"})
		assert.ElementsMatch(t, result.fieldContentsDiffer, []string{"c"})
	}
}

func TestBSONUnorderedCompareArrays(t *testing.T) {
	srcDoc := bson.D{
		{"_id", "a"},
		{"a", 1},
		{"b", 2},
		{"c", bson.D{{"d", 1}, {"e", 2}, {"f", 3}}},
		{"arr", bson.A{1, 2, 3, 4}}}
	result, err := compareDocuments(srcDoc, srcDoc)
	assert.NoError(t, err)
	assert.Nil(t, result)

	// Array is not equal to doc with elements that look like an array
	dstDoc := bson.D{
		{"_id", "a"},
		{"a", 1},
		{"b", 2},
		{"c", bson.D{{"d", 1}, {"e", 2}, {"f", 3}}},
		{"arr", bson.D{{"0", 1}, {"1", 2}, {"2", 3}, {"3", 4}}}}
	result, err = compareDocuments(srcDoc, dstDoc)
	assert.NoError(t, err)
	if assert.NotNil(t, result) {
		assert.Empty(t, result.missingFieldOnSrc)
		assert.Empty(t, result.missingFieldOnDst)
		assert.ElementsMatch(t, result.fieldContentsDiffer, []string{"arr"})
	}

	// Array with order changed
	dstDoc = bson.D{
		{"_id", "a"},
		{"a", 1},
		{"b", 2},
		{"c", bson.D{{"d", 1}, {"e", 2}, {"f", 3}}},
		{"arr", bson.A{1, 4, 3, 2}}}
	result, err = compareDocuments(srcDoc, dstDoc)
	assert.NoError(t, err)
	if assert.NotNil(t, result) {
		assert.Empty(t, result.missingFieldOnSrc)
		assert.Empty(t, result.missingFieldOnDst)
		assert.ElementsMatch(t, result.fieldContentsDiffer, []string{"arr"})
	}

	// Array with extra element
	dstDoc = bson.D{
		{"_id", "a"},
		{"a", 1},
		{"b", 2},
		{"c", bson.D{{"d", 1}, {"e", 2}, {"f", 3}}},
		{"arr", bson.A{1, 2, 3, 4, 5}}}
	result, err = compareDocuments(srcDoc, dstDoc)
	assert.NoError(t, err)
	if assert.NotNil(t, result) {
		assert.Empty(t, result.missingFieldOnSrc)
		assert.Empty(t, result.missingFieldOnDst)
		assert.ElementsMatch(t, result.fieldContentsDiffer, []string{"arr"})
	}

	// Array with missing element
	dstDoc = bson.D{
		{"_id", "a"},
		{"a", 1},
		{"b", 2},
		{"c", bson.D{{"d", 1}, {"e", 2}, {"f", 3}}},
		{"arr", bson.A{1, 2, 3}}}
	result, err = compareDocuments(srcDoc, dstDoc)
	assert.NoError(t, err)
	if assert.NotNil(t, result) {
		assert.Empty(t, result.missingFieldOnSrc)
		assert.Empty(t, result.missingFieldOnDst)
		assert.ElementsMatch(t, result.fieldContentsDiffer, []string{"arr"})
	}

	// Array with subdocument order changed should match
	srcDoc = bson.D{
		{"_id", "a"},
		{"a", 1},
		{"b", 2},
		{"c", bson.D{{"d", 1}, {"e", 2}, {"f", 3}}},
		{"arr", bson.A{1, bson.D{{"g", 4}, {"h", 5}, {"i", 6}}}}}
	dstDoc = bson.D{
		{"_id", "a"},
		{"a", 1},
		{"b", 2},
		{"c", bson.D{{"d", 1}, {"e", 2}, {"f", 3}}},
		{"arr", bson.A{1, bson.D{{"h", 5}, {"g", 4}, {"i", 6}}}}}
	result, err = compareDocuments(srcDoc, dstDoc)
	assert.NoError(t, err)
	assert.Nil(t, result)
}
