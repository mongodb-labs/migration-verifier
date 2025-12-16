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

## New generations

Between generations MV reads its recheck queue (i.e., its “notes to self”).
From this it creates recheck tasks, each of which contains a list of IDs of
documents to recheck.

MV also copies each document’s first mismatch time into the recheck tasks.
When those documents get rechecked, hopefully there is no more mismatch!
If there is, though, then we create another recheck & another mismatch,
populated as above.

# Change Events

Every time a document changes, either on the source or destination, MV
enqueues a recheck for that document. Unlike with mismatches, though,
such rechecks _do not_ contain a first-mismatch time. When MV turns such
rechecks into verifier tasks, there is no first-mismatch time for these in
the task.

Of particular note: the same document can have rechecks enqueued from both
a mismatch _and_ a change event. When this happens, MV “resets” the
document’s first-mismatch time by omitting it from the new generation’s
recheck task that contains the document’s ID.

For example: assume MV has seen a document mismatch for 1 minute. Then a
change event arrives at the same time that MV sees the mismatch again.
In the new generation, hopefully there is no more mismatch! If there is,
though, it will get a new first-mismatch time in the resulting recheck
& mismatch documents.
