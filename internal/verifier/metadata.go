package verifier

// Metadata version history:
// 1: Defined metadata version.
// 2: Split failed-task discrepancies into separate collection.
// 3: Enqueued rechecks now reference the generation in which they’ll be
//    rechecked rather than the generation during which they were enqueued.
// 4: Use “changeReader” instead of “changeStream” collection name.
// 5: Metadata now stores source & destination change reader options.
//    Also track mismatch duration.
// 6: Mismatches now record `generation` and `taskType`. `task` is now `taskID`.
// 7: Task format changed slightly to distinguish expected vs. found docs.

const verifierMetadataVersion = 7
