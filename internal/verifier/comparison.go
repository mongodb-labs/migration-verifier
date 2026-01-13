package verifier

import "github.com/10gen/migration-verifier/mslices"

type DocCompareMethod string
type DocQueryFunction string

const (
	DocCompareBinary           DocCompareMethod = "binary"
	DocCompareIgnoreOrder      DocCompareMethod = "ignoreOrder"
	DocCompareToHashedIndexKey DocCompareMethod = "toHashedIndexKey"

	DocQueryFunctionFind      DocQueryFunction = "find"
	DocQueryFunctionAggregate DocQueryFunction = "aggregate"
)

var (
	DocCompareMethods = mslices.Of(
		DocCompareBinary,
		DocCompareIgnoreOrder,
		DocCompareToHashedIndexKey,
	)

	DocCompareDefault = DocCompareMethods[0]
)

func (dcm DocCompareMethod) ShouldIgnoreFieldOrder() bool {
	return dcm == DocCompareIgnoreOrder
}

func (dcm DocCompareMethod) ComparesFullDocuments() bool {
	return dcm == DocCompareBinary || dcm == DocCompareIgnoreOrder
}
