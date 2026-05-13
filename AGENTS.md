# Tests

Use testify to write all test assertions. When testing errors, prefer
ErrorIs() and ErrorAs() tests; only check ErrorContains() as a last
resort. Such tests should assume strings will be reworded in the future.
Make those tests resist spurious breakage by limiting ErrorContains()
tests to checking for specific keywords and partial phrases that are
likely to be preserved in whatever rewording (or translation) may happen.

Avoid using f-suffixed assertions when non-f-suffixed will do. For example,
don’t do require.Equalf(t, 10, val, "must equal %d", 10) because Equal()
can do that just fine.

Avoid the True() assertion when there’s a more detailed assertion
available. For example, instead of:
```
	docMM, ok := resp["docMismatches"].(map[string]any)
	suite.Require().True(ok, "docMismatches should be an object, got %T", resp["docMismatches"])
```
… use the TypeIs() assertion.

In tests, prefer t.Context() to context.Background().

When writing tests, don’t describe fixed bugs directly; instead describe
the intended behavior. For example, don’t say:
```
// This ensures that the wrapped-error bug is fixed.
```
Instead, say something like:
```
// This ensures proper handling of wrapped errors.
```

# General coding practices

When writing string literals that contain double quotes (especially in test
assertions), use backticks instead of regular quotes to avoid needing
backslash escaping. For example, use `` `user="alice"` `` instead of
`"user=\"alice\""`.

When creating interface implementations, write out a compile-time check like:
```
var _ interfaceName = implementation{}
```

Use `mslices.Of()` where useful to avoid cluttering the code with types.
