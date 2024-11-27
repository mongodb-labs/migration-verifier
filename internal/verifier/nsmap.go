package verifier

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
		dstNs := dstNamespaces[i]
		if _, exist := nsmap.srcDstNsMap[srcNs]; exist {
			panic("another mapping already exists for source namespace " + srcNs)
		}
		if _, exist := nsmap.dstSrcNsMap[dstNs]; exist {
			panic("another mapping already exists for destination namespace " + dstNs)
		}
		nsmap.srcDstNsMap[srcNs] = dstNs
		nsmap.dstSrcNsMap[dstNs] = srcNs
	}
}

func (nsmap *NSMap) Len() int {
	if len(nsmap.srcDstNsMap) != len(nsmap.dstSrcNsMap) {
		panic("source and destination namespaces are not the same length")
	}

	return len(nsmap.srcDstNsMap)
}

func (nsmap *NSMap) GetDstNamespace(srcNamespace string) (string, bool) {
	ns, ok := nsmap.srcDstNsMap[srcNamespace]
	return ns, ok
}

func (nsmap *NSMap) GetSrcNamespace(dstNamespace string) (string, bool) {
	ns, ok := nsmap.dstSrcNsMap[dstNamespace]
	return ns, ok
}
