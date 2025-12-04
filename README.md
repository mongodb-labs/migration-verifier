# Verify Migrations!

_If verifying a migration done via [mongosync](https://www.mongodb.com/docs/cluster-to-cluster-sync/current/), please check if it is possible to use the
[embedded verifier](https://www.mongodb.com/docs/cluster-to-cluster-sync/current/reference/verification/embedded/#std-label-c2c-embedded-verifier) as that is the preferred approach for verification._

# Quick Start

Download the verifier’s latest release:
```
curl -sSL https://raw.githubusercontent.com/mongodb-labs/migration-verifier/refs/heads/main/download_latest.sh | sh
```
(Alternatively, you can check out this repository then `./build.sh` to build from source.)

Then start a local replica set to store verification metadata:
```
podman run -it --rm -p27017:27017 -v ./verifier_db:/data/db --entrypoint bash docker.io/mongodb/mongodb-community-server -c 'mongod --bind_ip_all --replSet rs & mpid=$! && until mongosh --eval "rs.initiate({_id: \"rs\", members: [{_id: 0, host: \"localhost:27017\"}]})"; do sleep 1; done && wait $mpid'
```
(This will create a local `verifier_db` directory so that you can resume verification if needed. Omit `-v` with its argument to avoid that.)

Finally, run verification:
```
./migration_verifier \
    --srcURI mongodb://your.source.cluster \
    --dstURI mongodb://your.destination.cluster \
    --serverPort 0 \
    --verifyAll \
    --start
```
The above will stream verification logs to standard output. Once writes stop,
watch for change stream lag to hit 0. The log will report either the found
mismatches or a confirmation of exact match between the clusters.

# Verifier Metadata Considerations

migration-verifier needs a MongoDB cluster to store its state. This cluster *must* support transactions (i.e., either a replica set or sharded cluster, NOT a standalone instance). By default, this is assumed to run on localhost:27017.

See [above](#Quick-Start) for a one-line command to start up a local, single-node replica set that you can use for this purpose.

The verifier can alternatively store its metadata on the destination cluster. This can severely degrade performance, though. Also, if you’re using mongosync, it requires either disabling mongosync’s destination write blocking or giving the `bypassWriteBlockingMode` to the verifier’s `--metaURI` user.

# More Details

To see all options:


```
./migration_verifier --help
```


To check all namespaces:


```
./migration_verifier --srcURI mongodb://127.0.0.1:27002 --dstURI mongodb://127.0.0.1:27003 --verifyAll
```


To check only specific namespaces:


```
./migration_verifier --srcURI mongodb://127.0.0.1:27002 --dstURI mongodb://127.0.0.1:27003 --srcNamespace foo.bar --dstNamespace foo.bar --srcNamespace foo.yar --dstNamespace foo.yar --srcNamespace mixed.namespaces --dstNamespace can.work
```


Note: that this will check from` foo.bar <-> foo.bar, foo.yar <-> foo.yar, `and` mixed.namespaces <-> can.work`

For checking namespaces with differing names between source and destination, the namespaces must be explicitly enumerated on the command line with the` --srcNamespace and --dstNamespace` flags. Names in the same position are considered to be the same:


```
migration-verifier … --srcNamespace foo.bar --srcNamespace foo.baz --dstNamespace foo.bar1 --dstNamespace bar.bar2 
```


will result in the mapping `foo.bar <-> foo.bar1, foo.baz <-> bar.bar2`

By default, the verifier will read from the primary node.  This can be changed with option “`--readPreference <preference>`” where `<preference>` can be “`primary`” (same as default), “`secondary`”, “`primaryPreferred`”, “`secondaryPreferred`”, or “`nearest`”.

To set a port, use `--serverPort <port number>`. The default is 27020. Note that migration-verifier listens on all available network interfaces, not just on `localhost`.

If you give 0 as the port, a random ephemeral port will be chosen. The log will show the chosen port, and you may also query the OS to learn it (e.g., `lsof -a -iTCP -sTCP:LISTEN -p <pid>`).

## Using a configuration file

To load configuration options from a YAML configuration file, use the `--configFile` parameter.

For example, you can specify `srcURI`, `dstURI`, and `metaURI` parameters thus:
```
---
srcURI: mongodb://localhost:28010
dstURI: mongodb://localhost:28011
metaURI: mongodb://localhost:28012
```


## Send the Verifier Process Commands:

1. After launching the verifier (see above), you can send it requests to get it to start verifying. If you don’t pass the `--start` parameter, verification is started by using the `check` command. An [optional `filter` parameter](#document-filtering) can be passed within the `check` request body to only check documents within that filter. The verification process will keep running until you tell the verifier to stop. It will keep track of the inconsistencies it has found and will keep checking those inconsistencies hoping that eventually they will resolve.

    ```
    curl -H "Content-Type: application/json" -d '{}' http://127.0.0.1:27020/api/v1/check
    ```


2. Once writes on the source cluster have stopped, you can tell the verifier that writes have stopped. (You can see the state of mongosync’s replication by hitting mongosync’s `progress` endpoint and checking that the state is `COMMITTED`. See the documentation [here](https://www.mongodb.com/docs/cluster-to-cluster-sync/current/reference/api/progress/#response)). \
The verifier will now check to completion to make sure that there are no inconsistencies. The command you need to send the verifier here is `writesOff`. The command doesn’t block. This means that you will have to poll the verifier, or watch its logs, to see the status of the verification (see `progress`).

    ```
    curl -H "Content-Type: application/json" -d '{}' http://127.0.0.1:27020/api/v1/writesOff
    ```


3. You can poll the status of the verification by hitting the `progress` endpoint. In particular, the `phase` should reveal whether the verifier is done verifying. Once the `phase` is `idle`, the verification has completed. At that point the `error` field should be `null`, and the `failedTasks` field should be `0`, if the verification was successful. A non-`null` `error` field indicates that the verifier itself ran into an error. `failedTasks` being non-`0` indicates that there was an inconsistency. See below for how to investigate mismatches.

```
curl http://127.0.0.1:27020/api/v1/progress
```

### `/progress` API Response Contents

In the below a “timestamp” is an object with `T` and `I` unsigned integers.
These represent a logical time in MongoDB’s replication protocol.

- `progress`
  - `phase` (string): either `idle`, `check`, or `recheck`
  - `generation` (unsigned integer)
  - `generationStats`
    - `timeElapsed` (string, [Go Duration format](https://pkg.go.dev/time#ParseDuration))
    - `activeWorkers` (unsigned integer)
    - `docsCompared` (unsigned integer)
    - `totalDocs` (unsigned integer)
    - `srcBytesCompared` (unsigned integer)
    - `totalSrcBytes` (unsigned integer, only present in `check` phase)
    - `mismatchesFound` (unsigned integer)
    - `rechecksEnqueued` (unsigned integer)
  - `srcChangeStats`
    - `eventsPerSecond` (nonnegative float, optional)
    - `currentTimes` (optional)
      - `lastHandledTime` (timestamp)
      - `lastClusterTime` (timestamp)
    - `bufferSaturation` (nonnegative float)
  - `dstChangeStats` (same fields as `srcChangeStats`)
  - `error` (string, optional)
  - `verificationStatus` (tasks for the current generation)
    - `totalTasks` (unsigned integer)
    - `addedTasks` (unsigned integer, unstarted tasks)
    - `processingTasks` (unsigned integer, in-progress tasks)
    - `failedTasks` (unsigned integer, tasks that found a document mismatch)
    - `completedTasks` (unsigned integer, tasks that found no problems)
    - `metadataMismatchTasks` (unsigned integer, tasks that found a collection metadata mismatch)


# CLI Options

| Flag                                    | Description                                                                                                                                                                                 |
|-----------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `--configFile <value>`                  | path to an optional YAML config file                                                                                                                                                        |
| `--srcURI <URI>`                        | source Host URI for migration verification (required)                                                                                                           |
| `--dstURI <URI>`                        | destination Host URI for migration verification (required)                                                                                                      |
| `--metaURI <URI>`                       | host URI for storing migration verification metadata (default: "mongodb://localhost")                                                                                                 |
| `--serverPort <port>`                   | port for the control web server (default: 27020)                                                                                                                                            |
| `--logPath <path>`                      | logging file path (default: "stdout")                                                                                                                                                       |
| `--numWorkers <number>`                 | number of worker threads to use for verification (default: 10)                                                                                                                              |
| `--generationPauseDelay <milliseconds>` | milliseconds to wait between generations of rechecking, allowing for more time to turn off writes (default: 1000)                                                                           |
| `--workerSleepDelay <milliseconds>`     | milliseconds workers sleep while waiting for work (default: 1000)                                                                                                                           |
| `--srcNamespace <namespaces>`           | source namespaces to check                                                                                                                                                                  |
| `--dstNamespace <namespaces>`           | destination namespaces to check                                                                                                                                                             |
| `--metaDBName <name>`                   | name of the database in which to store verification metadata (default: "migration_verification_metadata")                                                                                   |
| `--docCompareMethod`                    | How to compare documents. See below for details.                                                                                                                                        |
| `--start`                               | Start checking documents right away rather than waiting for a `/check` API request. |
| `--verifyAll`                           | If set, verify all user namespaces                                                                                                                                                          |
| `--clean`                               | If set, drop all previous verification metadata before starting                                                                                                                             |
| `--readPreference <value>`              | Read preference for reading data from clusters. May be 'primary', 'secondary', 'primaryPreferred', 'secondaryPreferred', or 'nearest' (default: "primary")                                  |
| `--partitionSizeMB <Megabytes>`         | Megabytes to use for a partition.  Change only for debugging. 0 means use partitioner default. (default: 0)                                                                                 |
| `--logLevel`                               | Set the logging to `info`, `debug`, or `trace` level.                                                                                                                                                                       |
| `--checkOnly`                           | Do not run the webserver or recheck, just run the check (for debugging)                                                                                                                     |
| `--failureDisplaySize <value>`          | Number of failures to display. Will display all failures if the number doesn’t exceed this limit by 25% (default: 20)                                                                       |
| `--ignoreReadConcern`                   | Use connection-default read concerns rather than setting majority read concern. This option may degrade consistency, so only enable it if majority read concern (the default) doesn’t work. |
| `--help`, `-h`                          | show help                                                                                                                                                                                   |

# Investigation of Mismatches

The verifier records any mismatches it finds in its metadata’s `mismatches`
collection. Mismatches are indexed by verification task ID. To find a given
generation’s mismatches, aggregate like this on the metadata cluster:

    // Change this as needed if you specify a custom metadata database:
    use migration_verification_metadata

    db.verification_tasks.aggregate(
        { $match: {
            generation: <whichever generation>,
            status: "failed",
        } },
        { $lookup: {
            from: "mismatches",
            localField: "_id",
            foreignField: "task",
            as: "mismatch",
        }},
        { $unwind: "$mismatch" },
    )

Note that each mismatch includes timestamps. You can cross-reference
these with the clusters’ oplogs to diagnose problems.

# Tests

This project’s tests run as normal Go tests, to, with `go test`.

`IntegrationTestSuite`'s tests require external clusters. You must provision these yourself.
(See the project’s GitHub CI setup for one way to simplify it.) Once provisioned, set the relevant
connection strings in the following environment variables:

- MVTEST_SRC
- MVTEST_DST
- MVTEST_META

# How the Verifier Works

The migration-verifier has two steps:



1. The initial check
    1. The verifier partitions up the data into 400MB (configurable) chunks and spins up many worker goroutines (threads) to read from both the source and destination.
    2. The verifier compares the documents on the source and destination by bytes and if they are different, it then checks field by field in case the field ordering has changed (since field ordering isn't required to be the same for the migration to be a success)

2. Iterative checking
    1. Since writes are coming in while the verification is happening, the verifier could both miss problematic changes made by the migration tool and could have temporary inconsistencies  that will clean themselves up later
    2. Before starting the initial check, the verifier starts a change stream on the source and keeps track of every document that is modified on the source
    3. In addition, the verifier keeps track of any documents that fail a check
    4. The verifier runs rounds of checks continuously until it is told that writes are off, fetching the documents stored from the change stream and that were inconsistent in the previous checking rounds, from both the source and destination and rechecking them. Once again, violations are written down for future checking rounds
    5. Every document to check is written with a generation number. A checking round checks documents for a specific generation. When a check round begins, we start writing new documents with a new generation number
    6. The verifier fetches all collection/index/view information on the source and destination and confirms they are identical in every generation. This is duplicated work, but it's fast and convenient for the code.

# Document Filtering

Document filtering can be enabled by passing a `filter` parameter in the `check` request body when starting a check. The filter takes a JSON query. The query syntax is identical to the [read operation query syntax](https://www.mongodb.com/docs/manual/tutorial/query-documents/#std-label-read-operations-query-argument). For example, running the following command makes the verifier check to only check documents within the filter `{"inFilter": {"$ne": false}}` for _all_ namespaces:

```
curl -H "Content-Type: application/json" -X POST -d '{{"filter": {"inFilter": {"$ne": false}}}}' http://127.0.0.1:27020/api/v1/check
```
If a checking is started with the above filter, the table below summarizes the verifier's behavior: 

| Source Document                                   | Destination Document                              | Verifier's Behavior                         |
|---------------------------------------------------|---------------------------------------------------|---------------------------------------------|
| `{"_id": <id>, "inFilter": true, "diff": "src"}`  | `{"_id": <id>, "inFilter": true, "diff": "dst"}`  | ❗ (Finds a document with differing content) |
| `{"_id": <id>, "inFilter": false, "diff": "src"}` | `{"_id": <id>, "inFilter": false, "diff": "dst"}` | ✅ (Skips a document)                        |
| `{"_id": <id>, "inFilter": true, "diff": "src"}`  | `{"_id": <id>, "inFilter": false, "diff": "dst"}` | ❗ (Finds a document missing on Destination) |
| `{"_id": <id>, "inFilter": false, "diff": "src"}` | `{"_id": <id>, "inFilter": true, "diff": "dst"}`  | ❗ (Finds a document missing on Source)      |

# Checking Failures

Because the algorithm is generational, the only failures we care about are in the last generation to run. The first goal is to find the last generation:

Switch to the verification database on the meta cluster, the default database for this is migration_verification_metadata:


```
use migration_verification_metadata
db.verification_tasks.aggregate([{$sort: {'generation': -1}}, {$limit: 1}, {$project: {'generation': 1, '_id': 0}}])
```


Once we have the generation, we can filter on generation:


```
x = //generation from previous query
db.verification_tasks.find({generation: x, status: 'failed'})
```


Failed 'verify' tasks will look like the following:


```
  {
    _id: ObjectId("632c994915c7f27f0a3de33e"),
    generation: 15,
    _ids: [ 3, ObjectId("632c994915c7f27f0a3de55e"), 5, "hello" ],
    id: 0,
    status: 'failed',
    type: 'verify',
    parent_id: null,
    query_filter: { partition: null, namespace: 'test.t', to: 'test.t' },
    begin_time: ISODate("2022-09-22T17:20:12.388Z"),
    failed_docs: null
  }
```


Type `'verify`' denotes that this is a mismatch in documents. `_ids` lists the `_ids` of failing documents, which may be of any bson type. The `query_filter` `'namespace'` field is the source namespace, while the `'to'` field is the namespace on the destination.

Any collection metadata mismatches will occur in a task with the type '`verifyCollection`', which looks like the following:


```
  x = 0
  db.verification_tasks.find({generation: x, status: 'mismatch'})
  {
    _id: ObjectId("632c9c9f5c71ad5eb4fde2bd"),
    generation: 0,
    _ids: null,
    id: 0,
    status: 'mismatch',
    type: 'verifyCollection',
    parent_id: null,
    query_filter: { partition: null, namespace: 'test.t', to: 'test.t' },
    begin_time: ISODate("2022-09-22T17:34:23.441Z"),
    failed_docs: [
      {
        id: 'x_1',
        field: null,
        type: null,
        details: 'Missing',
        cluster: 'dstClient',
        namespace: 'test.t'
      }
    ]
  },
```


In this case, '`failed_docs`' contains all the meta data mismatches, in this case an index named '`x_1`'.

# Resumability

The migration-verifier periodically persists its change stream’s resume token so that, in the event of a catastrophic failure (e.g., memory exhaustion), when restarted the verifier will receive any change events that happened while the verifier was down.

# Performance

The verifier has been observed handling test source write loads of 15,000 writes per second. Real-world performance will vary according to several factors, including network latency, cluster resources, and the verifier node’s resources.

## Change stream lag

Every time the verifier notices a change in a document, it schedules a recheck
of that document. If the changes happen faster than the verifier can schedule
rechecks, then the verifier “lags” the cluster. We measure that lag by
comparing the server-reported cluster time with the time of the most
recently-seen event.

If the lag exceeds a certain “comfortable” threshold, the verifier will warn
in the logs. High lag can cause either of these outcomes:

1. Once writes stop on the source (i.e., during the migration’s cutover),
you’ll have to wait for a longer-than-ideal time for the verifier to recheck
documents until its writes-off timestamp.

2. Sufficiently high verifier lag can exceed the server’s oplog capacity. If
this happens, verification will fail permanently, and you’ll have to restart
verification from the beginning.

### Mitigation

The following may help if you see warnings about change stream lag:

1. Scale up: Run the verifier on a more powerful host.

2. Reduce load: Disable nonessential applications during verification until cutover.

## Recheck generation size

Even if the change stream keeps up with the write load, the verifier may still recheck
the documents more slowly than writes happen on the source. If this happens, you’ll
see recheck generations grow over time.

Unlike change stream lag, this won’t actually endanger the verification. It will, though,
extend downtime during cutover because the final recheck generation will take longer than
it otherwise might.

### Mitigation

1. Scale up. (See above.)

2. Reduce load. (ditto)

3. Make the verifier compare document hashes rather than full documents. See below for details.

## Per-shard verification

If migrating shard-to-shard, you can also verify shard-to-shard to scale verification horizontally. Run 1 verifier per source shard. You can colocate all verifiers’ metadata on the same metadata cluster, but each verifier must use its own database (e.g., `verify90`, `verify1`, …). If that metadata cluster buckles under the load, consider splitting verification across multiple hosts.

# Document comparison methods

## `binary`

The default. This establishes full binary equivalence, including field order and all types.

## `ignoreFieldOrder`

Like `binary` but ignores the ordering of fields. Incurs extra overhead on the verifier host.

## `toHashedIndexKey`

Compares document hashes (and lengths) rather than full documents. This minimizes the data sent to migration-verifier, which can dramatically increase performance.

It carries a few downsides, though:

### Lost precision

This method ignores certain type changes if the underlying value remains the same. For example, if a Long changes to a Double, and the two values are identical, `toHashedIndexKey` will not notice the discrepancy.

The discrepancy _will_, though, usually be seen if the BSON types are of different lengths. For example, if a Long changes to Decimal, `toHashedIndexKey` will notice that.

If, however, _multiple_ numeric type changes happen, then `toHashedIndexKey` will only notice the discrepancy if the total document length changes. For example, if an Int changes to a Long, but elsewhere a Long changes to an Int, that will evade notice.

The above are all **highly** unlikely in real-world migrations.

### Lost reporting

Full-document verification methods allow migration-verifier to diagnose mismatches, e.g., by identifying specific changed fields. The only such detail that `toHashedIndexKey` can discern, though, is a change in document length.

Additionally, because the amount of data sent to migration-verifier doesn’t actually reflect the documents’ size, no meaningful statistics are shown concerning the collection data size. Document counts, of course, are still shown.

# Known Issues

- The verifier may report missing documents on the destination that don’t actually appear to be missing (i.e., a nonexistent problem). This has been hard to reproduce. If missing documents are reported, it is good practice to check for false positives.

- The verifier, during its first generation, may report a confusing “Mismatches found” but then report 0 problems. This is a reporting bug in mongosync; if you see it, check the documents in `migration_verification_metadata.verification_tasks` for generation 1 (not generation 0).

- The verifier conflates “missing” documents with change events: if it finds a document missing and receives a change event for another document, the verifier records those together in its metadata. As a result, the verifier’s reporting can cause confusion: what its log calls “missing or changed” documents aren’t, in fact, necessarily failures; they could all just be change events that are pending a recheck.

- If the server’s memory usage rises after generation 0, try reducing `recheckMaxSizeMB`. This will shrink the queries that the verifier sends, which in turn should reduce the server’s memory usage. (The number of actual queries sent will rise, of course.)

## Time-Series Collections

Because the verifier compares documents by `_id`, it cannot compare logical time-series measurements (i.e., the data that users actually insert). Instead it compares the server’s internal time-series “buckets”. Unfortunately, this makes mismatch details essentially useless with time-series since they will be details about time-series buckets, which users generally don’t see.

It also requires that migrations replicate the raw buckets rather than the logical measurements. This is because a logical migration would cause `_id` mismatches between source & destination buckets. A user application wouldn’t care (since it never sees the buckets’ `_id`s), but verification does.

NB: Given bucket documents’ size, hashed document comparison can be especially useful with time-series.

# Limitations

- The verifier’s iterative process can handle data changes while it is running, until you hit the writesOff endpoint.  However, it cannot handle DDL commands.  If the verifier receives a DDL change stream event from the source, the verification will fail permanently.

- The verifier cannot verify time-series collections under namespace filtering.
