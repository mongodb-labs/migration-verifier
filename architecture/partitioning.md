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
