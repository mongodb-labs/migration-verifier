# About this package

This package simplifies the use of MongoDB expressions, both in projections and aggregations.

See the godoc for usage details.

## Design principles

This library doesn’t cover all expressions for now. Expand as is convenient.

When doing so, **please observe the following:**

- Prefer `[1]any` for unary operators (e.g., `$bsonSize`).
- Prefer `[2]any` for binary operators whose arguments don’t benefit from naming (e.g., `$eq`).
- Prefer struct types for operators with named parameters AND for operators whose documentation
  gives names, even if those names aren’t sent to the server.
- Struct field names should match the server’s.
- Use functions sparingly, e.g., for “tuple” operators like `$in`.
- Use Go type `any` for arbitrary expressions.

Every expression type has a `D()` method that returns its `bson.D` representation and implements
`bson.Marshaler` through that method.

## Additional links

See [this package](https://github.com/mongodb-labs/mongo-go-driver-exp/tree/main/mql) for a similar
effort that may become officially part of the Go driver eventually. If it does, most or all of this
package may go away.
