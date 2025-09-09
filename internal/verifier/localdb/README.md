# Migration Verifier: Local DB

Migration Verifier stores some of its state not in its configured
metadata database, but in a local key-value datastore. This minimizes
network latency’s effect on verification and makes it much more feasible
to store Migration Verifier’s metadata on the destination cluster.

This local DB is built atop
[BadgerDB](https://docs.hypermode.com/badger/overview).

This document describes the local DB’s design.

## Change stream resume tokens

Both the source & destination resume tokens are stored in this
datastore. They are stored as `metadata.srcCSResumeToken` and
`metadata.dstCSResumeToken`.

## Rechecks

This is much trickier. :-/

We need to avoid persisting duplicate rechecks. Ideally we could just
use the document ID as part of the key, but BadgerDB limits keys to
1 KiB. Thus, we store a hash of the document ID, rather than the ID
itself, in the document key.

Recheck keys look thus:
```
recheck-<generation>-<namespace length>-<namespace><doc ID hash>
```
Note that we store a namespace length to avoid needing to parse out
an end to the namespace. (We could use `$`, but if the server ever starts
allowing that character in namespaces, our parsing logic would break.)

Since multiple document IDs can share the same hash, the actual
value stored in the BadgerDB item is a sequence of BSON documents.
