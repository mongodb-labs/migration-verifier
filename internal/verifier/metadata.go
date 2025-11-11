package verifier

// Metadata version history:
// 1: Defined metadata version.
// 2: Split failed-task discrepancies into separate collection.
// 3: Enqueued rechecks now reference the generation in which they’ll be
//    rechecked rather than the generation during which they were enqueued.
// 4: changeStream -> changeReader

const verifierMetadataVersion = 4
