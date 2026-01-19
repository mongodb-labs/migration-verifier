# Comparison

This document describes Migration Verifier’s document comparison mechanism.

## Overview

Broadly speaking, there are 3 channels: one to read documents from the source,
another to read documents from the destination, and a third that does the
actual comparison. The document-reader threads feed the comparison thread
(“comparator”).

## Memory usage

All mismatches for a given task are buffered in memory. Once the task
finishes, the task thread—i.e., the thread that invokes the comparison—writes
those mismatches to Verifier’s metadata and marks the task as either completed
(if there are no mismatches) or “failed” (if there are mismatches).

To minimize memory consumption, the comparator always reads documents
from alternate readers: one from the source, then one from the destination.
For each received document, the thread checks to see if its reader’s “peer”
has already gotten that document (matched by `_id` via a binary match). If
so, then those 2 documents are compared, and any mismatch is stored for
later reporting. If the document has *not* been seen, then it gets buffered
for later checks.

Once the readers have finished sending documents to the comparator,
the comparator marks any documents still in its buffer as
missing/extra.

To minimize GC (garbage collection) pressure, a memory pool is used to copy
documents from the cursors. The comparator frees that memory either
after comparing documents or after marking them as missing/extra.

## Generation 0: Partitioning by document `_id`

Under document `_id` partitioning, the source & destination reader threads
operate independently. They both read documents in ascending `_id` order.
As a result, the comparator will see the same documents in the same
order from both source & destination, which minimizes memory usage.

Once either reader’s cursor finishes, that reader closes its channel to
the comparator.

## Generation 0: Natural partitioning

Natural partitioning is more complicated than `_id` partitioning.
Under this scheme, the source & destination threads are **not**
independent.

The source reader reads documents in natural order. As with `_id`
partitioning, it sends those documents to the comparator.

It also, though, sends those documents’ `_id`s to the destination reader.
That reader then fetches the relevant documents—by `_id`—from the
destination and sends them to the comparator.

Buffering prevents blocking here: the source is always ahead of the
destination, even though the comparator still compares their results
together.

Also note that, unlike with `_id` partitioning, the destination here opens a
separate cursor for each  batch of `_id`s it receives from the source. This
means we cannot close the dst->comparator channel after a cursor closes.
Instead, the destination reader counts the number of `_id`s it received from
the source and subtracts the number of documents read from the destination.
It then sends that many “dummy” documents to the comparator.

### Direct connection requirement

Because the source reader queries the source by record ID, the reader
**MUST** read from the same node that Verifier connected to when creating
the task’s partition. The partition, thus, stores that hostname & port,
which the reader connects to via modification to the source connection string
initially given to Migration Verifier

## Comparison methods

### Binary comparison

Not much to say … we compare two buffers, and if they mismatch, report that.

## Ignoring field order

Migration Verifier implements custom logic for this.

## $toHashedIndexKey

In this mode Verifier compares just 2 integers for each document: the BSON
length, and an int64 gotten from `$toHashedIndexKey`.

`$toHashedIndexKey` has a problem, though: high-value floats all yield the
same hash. To avoid this, before computing the document’s hash we convert
the document, via an undocumented aggregation operator, to its binary
“key string” representation.
