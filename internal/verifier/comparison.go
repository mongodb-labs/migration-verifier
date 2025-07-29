package verifier

import (
	mapset "github.com/deckarep/golang-set/v2"
)

type DocCompareMethod string
type DocQueryFunction string

const (
	DocCompareBinary           DocCompareMethod = "binary"
	DocCompareIgnoreOrder      DocCompareMethod = "ignoreOrder"
	DocCompareToHashedIndexKey DocCompareMethod = "toHashedIndexKey"

	DocQueryFunctionFind      DocQueryFunction = "find"
	DocQueryFunctionAggregate DocQueryFunction = "aggregate"
)

var DocCompareMethods = mapset.NewSet(
	DocCompareBinary,
	DocCompareIgnoreOrder,
	DocCompareToHashedIndexKey,
)

func (dcm DocCompareMethod) ShouldIgnoreFieldOrder() bool {
	return dcm == DocCompareIgnoreOrder
}

func (dcm DocCompareMethod) ComparesFullDocuments() bool {
	return dcm == DocCompareBinary || dcm == DocCompareIgnoreOrder
}

func (dcm DocCompareMethod) QueryFunction() DocQueryFunction {
	switch dcm {
	case DocCompareBinary, DocCompareIgnoreOrder:
		return DocQueryFunctionFind
	case DocCompareToHashedIndexKey:
		return DocQueryFunctionAggregate
	default:
		panic("Unknown doc compare method: " + dcm)
	}
}
