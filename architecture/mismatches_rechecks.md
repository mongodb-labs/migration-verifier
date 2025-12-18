# Mismatches & Rechecks

When Migration Verifier (MV) sees a mismatch, it records two documents:

1. a recheck (i.e., in the recheck queue)
2. a mismatch

The recheck is a “note to self” that the document needs to be rechecked
in the next generation. The mismatch records details about the actual mismatch,
which informs logging.

Both of these contain the first time that the document was seen to mismatch.
(If this is the first time we’ve seen the mismatch, then that time is “now”.)
The mismatch additionally contains the length of time since a) that first time,
and b) the most recent time the mismatch was seen.

# Change Events

Every time a document changes, either on the source or destination, MV
enqueues a recheck for that document. Unlike with mismatches, though,
such rechecks _do not_ contain a first-mismatch time. Instead, these rechecks
contain a) the change event’s ClusterTime, and b) an indicator of which
cluster had the change (currently implemented as a boolean).

The cluster time & cluster indicator allow MV to track the latest change
event it has fully rechecked. (See below.)

# New generations

Between generations MV reads its recheck queue (i.e., its “notes to self”).
From this it creates recheck tasks, each of which contains a list of IDs of
documents to recheck.

Each recheck task records each document ID’s, the first-mismatch time.
It also stores the latest source & destination change timestamps seen among
the rechecks.

Of particular note: the same document can have rechecks enqueued from both
a mismatch _and_ change events. When this happens, MV “resets” the
document’s first-mismatch time by omitting it from the new generation’s
recheck task.

For example: assume MV has seen a document mismatch for 1 minute. Then a
change event arrives at the same time that MV sees the mismatch again.
In the new generation, hopefully there is no more mismatch! If there is,
though, it will get a new first-mismatch time in the resulting recheck
& mismatch documents.

# Recheck task processing

When MV processes recheck tasks, it sends a big `{_id: {$in: [...]}}` query
to both source & destination clusters. It then matches up the results by
document ID (binary equivalence).

If both clusters returned a document, then we compare them. If they mismatch,
a mismatch recheck is enqueued as described above.

Once all documents are compared, verifier reads the task’s source &
destination timestamps then updates its internal last-compared timestamps.
