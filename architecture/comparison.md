# Comparison

This document describes Migration Verifier’s document comparison mechanism.

## Overview

Broadly speaking, there are 3 channels: one to read documents from the source,
another to read documents from the destination, and a third that does the
actual comparison. The document-reader threads feed the comparison thread
(“comparator”).

## Memory & CPU usage

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

### Batching

If the readers sent documents individually to the comparator, there would be
high contention among the threads due to a large number of messages being sent.

At the same time, if the batches grow too big, memory usage will spike, which
would create GC pressure.

Verifier strikes a balance, then, by setting a static limit on the number of
documents sent to the comparator at once.

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

### Natural batch flushing

In `_id` partitioning the source & destination reader channels send documents
until their cursors finish. They then close their channels. At that point we
know that any missing documents can be reported as mismatches.

With pre-v4.4 sources, natural-order scans are unpartitioned, though, so the
source’s cursor doesn’t close until it has iterated an entire collection. The
comparator needs some boundary, though, to flush mismatches, or else it’ll
buffer them in memory indefinitely. To achieve this, the source & destination
both send notifications to the comparator when they have finished reading the
documents for a batch of source document `_id`s. The comparator can safely
flush mismatches when it sees that message from both readers.

Note that either reader can send this batch-end message first. The typical case,
of course, is where the source has documents that are not replicated to the
destination, in which case the destination will send batch-end first. In RS->SC
migrations, though, the destination can store up to 1 document per shard for a
given `_id`; thus, the destination’s document count could exceed the source’s.
In that case the source could send batch-end before the destination. (In
practice this should be extremely rare since duplicate `_id`s will be transient
artifacts of change event replication.)

In either case, a batch-end from one reader causes the comparator to stop
reading from that reader until it receives the other reader’s batch-end. The
batch-end message itself is an incrementing u64; the comparator asserts that
the source & destination messages match.

### Direct connection requirement

Because the source reader queries the source by record ID, the reader
**MUST** read from the same node that Verifier connected to when creating
the task’s partition. The partition, thus, stores that hostname & port,
which the reader connects to via modification to the source connection string
initially given to Migration Verifier

## Comparison methods

### Binary comparison

Not much to say … we compare two buffers, and if they mismatch, report that.

### Ignoring field order

Migration Verifier implements custom logic for this.

### $toHashedIndexKey

In this mode Verifier compares just 2 integers for each document: the BSON
length, and an int64 gotten from `$toHashedIndexKey`.

`$toHashedIndexKey` has a problem, though: high-value floats all yield the
same hash. To avoid this, before computing the document’s hash we convert
the document, via an undocumented aggregation operator, to its binary
“key string” representation.
