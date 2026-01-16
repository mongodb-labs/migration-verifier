# Partitioning

Migration Verifier parallelizes its initial scan of the cluster data. This
yields 2 major benefits: it’s faster, and it’s resumable. (i.e., after a
restart we can resume verification without losing all prior progress)

To achieve this, each collection in generation 0 has 1 or more
document-verification tasks. Each such task defines a range of documents
to verify between source & destination clusters.

The following document describes this system’s implementation.

## Partitioning by `_id`

By default Migration Verifier partitions by document `_id`. Thus,
for a given collection we might see these partitions:

- MinKey to 500
- 500 to Timestamp{12, 23}
- Timestamp{12, 23} to MaxKey

(See [here](https://www.mongodb.com/docs/manual/reference/bson-type-comparison-order/)
for more details on BSON sort order.)

These get stored into Partition structs, which in turn are stored
into Task structs.

Note that MinKey & MaxKey are stored literally in the Partition.

### Sampling

To split a collection up, Verifier queries for a random set of documents.
Historically the `$sample` aggregation operator was used for this; however,
that operator had a habit of yielding one or more over-large partitions.

In MongoDB 4.4 & later Verifier now uses the `$sampleRate` operator. This
takes longer to finish, so to compensate Verifier does the partitioning
concurrently with the actual document verification: once the first
partition task is created, Verifier starts comparing its documents
while other partition tasks are still being created.

### Querying across types

A standard MongoDB `find` query will not behave as you’d expect if you
feed it a filter like:
```
{
    _id: { $and: [
        {$gte: 500},
        {$lte: {foo: 1}},
    ] },
}
```
Such a query will only return numeric or embedded-doc results. This is so
despite that, per BSON’s sort order (see link above), BSON symbol & string
values “should” be part of the result.

The reason for this is “type bracketing”: `find` only matches types
that actually match one of the search criteria.

This behavior is unacceptable for verification. In this case, if there are
string or symbol `_id`s, Verifier would quietly skip those documents!

There are 2 ways around this problem:

#### Aggregation expressions (`$expr`)

Aggregation expressions _do not_ bracket types when matching values.
So the expression `{$lte: [123, TimeStamp{1, 2}]}` will evaluate to true.
Thus, if we use `$expr` in our queries rather than plain `find` operators,
we have correct logic.

In some old MongoDB versions, though, the query optimizer can’t parse
$expr efficiently, which causes a full collection scan for such queries.

#### Manual type joining

We can alternatively work within the type bracketing by explicitly
including the intermediate types (i.e., string & symbol in our above example).
This looks like:
```
{
    _id: { $or: [
        {$gte: 500},
        {$type: ["string", "symbol"]},
        {$lte: {foo: 1}},
    ] }
```
This is rather ugly, but it avoids skipped documents without needing a
full collection scan.

## Partitioning by record ID (“natural” partitioning)

When partitioning by record ID, the bounds stored are:

- Lower: BSON null for the first partition; afterwards a resume token
- Upper: a record ID or, for empty collections, BSON null

### Record ID scope

Unlike a document’s `_id`, its record ID is an artifact of storage on a
particular node in a replica set. A document with `{_id: 234}` might have
a record ID of 2 on one node while on another node that document’s record
ID is 48.

Because of this, Verifier **MUST** fulfill natural-partition tasks by
fetching documents from the same node it connected to during partitioning.

We achieve this by storing the node’s hostname in the task itself. Then,
when Verifier executes the task, it opens a direct connection to that
specific node in order to read the documents.

### Querying & document deletions

MongoDB doesn’t expose a control for querying a range of record IDs.
This is why lower bounds are resume tokens—which contain record IDs—rather
than simple record IDs: MongoDB _will_ resume from a token.

There’s a problem, though. Assume that the document that a given resume
token refers to has been _deleted_. Historically, the server always returned
an error in this case.

To handle this error, Verifier decrements the record ID and resends the
request. Eventually there will either be a “hit” (i.e., a record ID that
refers to a still-existing document), or we’ll reach record ID of 0, which
means we can just start from the beginning of the collection.

(This can yield a substantial “flurry” of requests until the cursor is
opened.)

The above doesn’t work for all record IDs, though. See below.

In 7.0 & later the server’s `find` command accepts an alternate parameter,
`$_startAt`, that will, in this circumstance, instead seek to the next
existing document and start the cursor there.

### What’s in a record ID?

For ordinary collections the record IDs are BSON int64. These can be
decremented as described above to handle deletions.

For clustered collections, though, the record IDs are binary strings.
These can’t be meaningfully decremented as int64 record IDs can. Thus,
only source versions whose `find` supports `$_startAt` can partition
a clustered collection naturally.

### When partitioning can’t happen

Besides the clustered-collection case, pre-4.4 sources and sharded
clusters are other cases where natural partitioning doesn’t work.

Natural _scanning_, however, is still valuable for fetching the
documents to avoid the high read amplification that can happen from
ID partitioning.

Thus, in these cases where natural partitioning is chosen but,
for whatever reason, doesn’t work, Verifier puts the entire collection
into a single task. This task will be processed via natural scanning.
Because there is only 1 task, though, the collection’s verification
is not resumable: if a failure happens while verifying the collection,
the collection’s verification has to be restarted. This will also be
slower than parallelizing the verification, though it may still
outperform ID partitioning.

### Why upper bound is only null for empty collections

One might think that, because the first partition’s lower bound is
BSON null, the last partition’s upper bound should be null as well.
This, though, would make the last partition take over-long with no benefit.

Consider a collection with frequent document insertions & deletions. Assume
its top record ID is 1,000,000 at the start of partitioning. Assume that the
last partition is not processed until near the end of a days-long generation 0.
Over those days a steady stream of insertions arrives. Each of those increments
the collection’s top record ID. Thus, by the time that last partition is
processed, the top record ID might be 2,000,000. This would mean that that
single partition will encompass half of the record ID space!

We prevent this by querying the top record ID at partitioning time and
setting that as the last partition’s upper bound. Since generation 1 will
recheck all documents inserted during generation 0, generation 0 doesn’t need
to compare those documents.

## No partitioning?

If a given collection is not large enough for even a single partition
(as per the configured partition size), then a single document-comparison
task is created for it. This task sets MinKey & MaxKey as its `_id`
boundaries.
