# Verify Migrations!

_This Repository is NOT an officially supported MongoDB product. This tool reduces the risk of undetected data inconsistencies from migrations, but it doesn’t provide guarantees of correctness._

# To build


```
go build main/migration_verifier.go
```



# Operational UX Once Running 

_Assumes no port set, default port for operation webserver is 27020_


## Launch the Verifier Binary

To see all options: 


```
./migration_verifier --help
```


To check all namespaces: 


```
./migration_verifier --srcURI mongodb://127.0.0.1:27002 --dstURI mongodb://127.0.0.1:27003 --metaURI mongodb://127.0.0.1:27001 --metaDBName verify_meta --verifyAll  
```


To filter namespaces (allow list): 


```
./migration_verifier --srcURI mongodb://127.0.0.1:27002 --dstURI mongodb://127.0.0.1:27003 --metaURI mongodb://127.0.0.1:27001 --metaDBName verify_meta --srcNamespace foo.bar --dstNamespace foo.bar --srcNamespace foo.yar --dstNamespace foo.yar --srcNamespace mixed.namespaces --dstNamespace can.work
```


Note: that this will check from` foo.bar <-> foo.bar, foo.yar <-> foo.yar, `and` mixed.namespaces <-> can.work`

For checking namespaces with differing names between source and destination, the namespaces must be explicitly enumerated on the command line with the` --srcNamespace and --dstNamespace` flags. Names in the same position are considered to be the same:


```
migration-verifier … --srcNamespace foo.bar --srcNamespace foo.baz --dstNamespace foo.bar1 --dstNamespace bar.bar2 
```


will result in the mapping `foo.bar <-> foo.bar1, foo.baz <-> bar.bar2`

By default, the verifier will read from the primary node.  This can be changed with option “`--readPreference <preference>`” where `<preference>` can be “`primary`” (same as default), “`secondary`”, “`primaryPreferred`”, “`secondaryPreferred`”, or “`nearest`”.  

To set a port, use `--serverPort <port number>`. The default is 27020.

### Using a configuration file

To load configuration options from a YAML configuration file, use the `--configFile` parameter.

For example, you can specify `srcURI`, `dstURI`, and `metaURI` parameters thus:
```
---
srcURI: mongodb://localhost:28010
dstURI: mongodb://localhost:28011
metaURI: mongodb://localhost:28012
```


## Send the Verifier Process Commands: 



1. After launching the verifier (see above), you can send it requests to get it to start verifying. The verification process is started by using the `check`command. An [optional `filter` parameter](#document-filtering) can be passed within the `check` request body to only check documents within that filter. The verification process will keep running until you tell the verifier to stop. It will keep track of the inconsistencies it has found and will keep checking those inconsistencies hoping that eventually they will resolve.

    ```
    curl -H "Content-Type: application/json" -X POST -d '{}' http://127.0.0.1:27020/api/v1/check
    ```


2. Once mongosync has committed the replication, you can tell the verifier that writes have stopped. You can see the state of mongosync’s replication by hitting mongosync’s `progress` endpoint and checking that the state is `COMMITTED`. See the documentation [here](https://www.mongodb.com/docs/cluster-to-cluster-sync/current/reference/api/progress/#response). \
The verifier will now check to completion to make sure that there are no inconsistencies. The command you need to send the verifier to tell it that the replication is committed is `writesOff`. The command doesn’t block. This means that you will have to poll the verifier to see the status of the verification (see `progress`).

    ```
    curl -H "Content-Type: application/json" -X POST -d '{}' http://127.0.0.1:27020/api/v1/writesOff
    ```


3. You can poll the status of the verification by hitting the `progress`endpoint. In particular, the `phase`should reveal whether the verifier is done verifying; once the `phase`is `idle`the verification has completed. When the `phase`has reached `idle`, the `error`field should be `null`and the `failedTasks`field should be `0`, if the verification was successful. A non-`null``error`field indicates that the verifier itself ran into an error. `failedTasks`being non-`0`indicates that there was an inconsistency. The logs printed by the verifier itself should have more information regarding what the inconsistencies are.

    ```
    curl -H "Content-Type: application/json" -X GET http://127.0.0.1:27020/api/v1/progress

    ```


	

	This is a sample output when inconsistencies are present:


    	`{"progress":{"phase":"idle","error":null,"verificationStatus":{"totalTasks":1,"addedTasks":0,"processingTasks":0,"failedTasks":1,"completedTasks":0,"metadataMismatchTasks":0,"recheckTasks":0}}}`


# CLI Options

| Flag                                    | Description                                                                                                                                                                                 |
|-----------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `--configFile <value>`                  | path to an optional YAML config file                                                                                                                                                        |
| `--srcURI <URI>`                        | source Host URI for migration verification (default: "mongodb://localhost:27017")                                                                                                           |
| `--dstURI <URI>`                        | destination Host URI for migration verification (default: "mongodb://localhost:27018")                                                                                                      |
| `--metaURI <URI>`                       | host URI for storing migration verification metadata (default: "mongodb://localhost:27019")                                                                                                 |
| `--serverPort <port>`                   | port for the control web server (default: 27020)                                                                                                                                            |
| `--logPath <path>`                      | logging file path (default: "stdout")                                                                                                                                                       |
| `--numWorkers <number>`                 | number of worker threads to use for verification (default: 10)                                                                                                                              |
| `--generationPauseDelay <milliseconds>` | milliseconds to wait between generations of rechecking, allowing for more time to turn off writes (default: 1000)                                                                           |
| `--workerSleepDelay <milliseconds>`     | milliseconds workers sleep while waiting for work (default: 1000)                                                                                                                           |
| `--srcNamespace <namespaces>`           | source namespaces to check                                                                                                                                                                  |
| `--dstNamespace <namespaces>`           | destination namespaces to check                                                                                                                                                             |
| `--metaDBName <name>`                   | name of the database in which to store verification metadata (default: "migration_verification_metadata")                                                                                   |
| `--ignoreFieldOrder`                    | Whether or not field order is ignored in documents                                                                                                                                          |
| `--verifyAll`                           | If set, verify all user namespaces                                                                                                                                                          |
| `--clean`                               | If set, drop all previous verification metadata before starting                                                                                                                             |
| `--readPreference <value>`              | Read preference for reading data from clusters. May be 'primary', 'secondary', 'primaryPreferred', 'secondaryPreferred', or 'nearest' (default: "primary")                                  |
| `--partitionSizeMB <Megabytes>`         | Megabytes to use for a partition.  Change only for debugging. 0 means use partitioner default. (default: 0)                                                                                 |
| `--debug`                               | Turn on debug logging                                                                                                                                                                       |
| `--checkOnly`                           | Do not run the webserver or recheck, just run the check (for debugging)                                                                                                                     |
| `--failureDisplaySize <value>`          | Number of failures to display. Will display all failures if the number doesn’t exceed this limit by 25% (default: 20)                                                                       |
| `--ignoreReadConcern`                   | Use connection-default read concerns rather than setting majority read concern. This option may degrade consistency, so only enable it if majority read concern (the default) doesn’t work. |
| `--help`, `-h`                          | show help                                                                                                                                                                                   |


# Benchmarking Results

Ran on m6id.metal + M40 with 3 replica sets

Command run python3 ./test/benchmark.py --way=recheck remote

When running with 1TB of random data on 3 collections

**In recheck and normal mode it runs at 1.5-2.5gbps per replica** and is **disk bound on each node** (meaning there are not of easy optimizations to make this faster) \
On default settings it used about **200GB of RAM on m6id.metal machine when using all the cores**

**This means it does about 1TB/20min but it is HIGHLY dependent on the source and dest machines**


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

**CAVEAT:** This doesn’t work properly during partitionining. If the verifier ends prior to partitioning, you should restart the verification.

# Performance

The migration-verifier optimizes for the case where a migration’s initial sync is completed **and** change events are relatively infrequent. If you start verification before initial sync finishes, or if the source cluster is too busy, the verification may freeze.

The migration-verifier is also rather resource-hungry. To mitigate this, try limiting its number of workers (i.e., `--numWorkers`), its partition size (`--partitionSizeMB`), and/or its process group’s resource limits (see the `ulimit` command in POSIX OSes).

# Known Issues

- The verifier may report missing documents on the destination that don’t actually appear to be missing (i.e., a nonexistent problem). This has been hard to reproduce. If missing documents are reported, it is good practice to check for false positives.

- The verifier, during its first generation, may report a confusing “Mismatches found” but then report 0 problems. This is a reporting bug in mongosync; if you see it, check the documents in `migration_verification_metadata.verification_tasks` for generation 1 (not generation 0).

- The verifier conflates “missing” documents with change events: if it finds a document missing and receives a change event for another document, the verifier records those together in its metadata. As a result, the verifier’s reporting can cause confusion: what its log calls “missing or changed” documents aren’t, in fact, necessarily failures; they could all just be change events that are pending a recheck.

# Limitations

- The verifier’s iterative process can handle data changes while it is running, until you hit the writesOff endpoint.  However, it cannot handle DDL commands.  If the verifier receives a DDL change stream event (drop, dropDatabase, rename), the verification will fail.  If an untracked DDL event (create, createIndexes, dropIndexes, modify) occurs, the verifier may miss the change.

- The verifier crashes if it tries to compare time-series collections. The error will include a phrase like “Collection has nil UUID (most probably is a view)” and also mention “timeseries”.
