# The `dockeys` package

This package is responsible for computing document keys from full documents. A _document key_
uniquely identifies a document: for replica sets, the document key is always just the `_id`, and in
sharded clusters, it is the `_id` + shard key. The verifier uses document keys to key the maps in
the aggregator: we need to do this because keying by `_id` is not sufficient, since the same `_id`
can exist on multiple shards with different shard keys. (This is not merely academic: mongosync
creates these transient duplicate `_id`s as part of its normal operation.)

The logic in this package duplicates the server's logic for generating document keys. Ideally we
_wouldn't_ do this, and could rely on the server to generate them for us in all cases, but right now
there is no way to do that. (The verifier must generate document keys from documents at rest during
the initial collection scan.)

To generate the document key for a document, pass the shard key pattern and the full document to
`ExtractDocumentKey`. That function will first iterate through the shard key and extract all of the
relevant field values from the document, if they are present. If the `_id` is not a part of the
shard key pattern, it also appends the `_id` field.

**Important:** the tests for this package are in `verifier/dockeys_test`. They use all of the
verifier's test abstractions, and so it's easier for now to keep them there. (When mongosync and the
verifier share more test abstractions, it might be useful to move them.)
