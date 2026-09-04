package upgrade

// CheckVersion checks whether a new version is available.
func CheckVersion(version string) (*Info, error) {
	return inspect(version)
}
