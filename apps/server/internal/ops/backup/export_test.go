package backup

// ExtractForTest exposes extractTar to the black-box tests.
func ExtractForTest(archive, dest string) error { return extractTar(archive, dest, true) }
