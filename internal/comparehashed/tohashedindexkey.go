package comparehashed

func CanCompareDocsViaToHashedIndexKey(
	version []int,
) bool {
	if version[0] >= 8 {
		return true
	}

	switch version[0] {
	case 7:
		return version[2] >= 6
	case 6:
		return version[2] >= 14
	case 5:
		return version[2] >= 25
	case 4:
		return version[1] == 4 && version[2] >= 29
	default:
		return false
	}
}
