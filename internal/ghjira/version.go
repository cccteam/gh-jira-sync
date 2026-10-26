package ghjira

import "runtime/debug"

// releaseVersion is set by release builds with -ldflags "-X ...releaseVersion=vX.Y.Z".
var releaseVersion string

// Version is the release the binary was built from: set by release builds, or read
// from the module by `go install github.com/cccteam/gh-jira-sync@vX.Y.Z`. A local
// `go build` reports a VCS pseudo-version, and `go run` reports "(devel)".
func Version() string {
	if releaseVersion != "" {
		return releaseVersion
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}

	return "(devel)"
}
