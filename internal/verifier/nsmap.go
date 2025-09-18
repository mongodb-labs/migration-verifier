package verifier

import "fmt"

type NSMap struct {
	srcDstNsMap map[string]string
	dstSrcNsMap map[string]string
}

func NewNSMap() *NSMap {
	return &NSMap{
		srcDstNsMap: make(map[string]string),
		dstSrcNsMap: make(map[string]string),
	}
}

func (nsmap *NSMap) PopulateWithNamespaces(srcNamespaces []string, dstNamespaces []string) {
	if len(srcNamespaces) != len(dstNamespaces) {
		panic("source and destination namespaces are not the same length")
	}

	for i, srcNs := range srcNamespaces {
		if err := nsmap.Augment(srcNs, dstNamespaces[i]); err != nil {
			panic(err.Error())
		}
	}
}

func (nsmap *NSMap) Len() int {
	if len(nsmap.srcDstNsMap) != len(nsmap.dstSrcNsMap) {
		panic("source and destination namespaces are not the same length")
	}

	return len(nsmap.srcDstNsMap)
}

func (nsmap *NSMap) Augment(srcNs, dstNs string) error {
	if _, exist := nsmap.srcDstNsMap[srcNs]; exist {
		return fmt.Errorf("another mapping already exists for source namespace %#q", srcNs)
	}
	if _, exist := nsmap.dstSrcNsMap[dstNs]; exist {
		return fmt.Errorf("another mapping already exists for destination namespace %#q", dstNs)
	}

	nsmap.srcDstNsMap[srcNs] = dstNs
	nsmap.dstSrcNsMap[dstNs] = srcNs

	return nil
}

func (nsmap *NSMap) GetDstNamespace(srcNamespace string) (string, bool) {
	ns, ok := nsmap.srcDstNsMap[srcNamespace]
	return ns, ok
}

func (nsmap *NSMap) GetSrcNamespace(dstNamespace string) (string, bool) {
	ns, ok := nsmap.dstSrcNsMap[dstNamespace]
	return ns, ok
}
