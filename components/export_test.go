package components

// ManifestImageSlots is how many manifests the image scan currently remembers.
func ManifestImageSlots() int {
	n := 0
	manifestImages.Range(func(_, _ any) bool { n++; return true })
	return n
}
