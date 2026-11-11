package main

import "runtime/debug"

// version and commit are set at link time with
// -ldflags "-X main.version=... -X main.commit=..." by the Makefile and the
// release workflow. Builds that do not set them fall back to the build
// information recorded by the Go toolchain (see versionFromBuildInfo).
var (
	version = "dev"
	commit  = "none"
)

// versionFromBuildInfo fills in values that were not set at link time from the
// build information embedded by the Go toolchain:
//
//   - `go install github.com/starriv/splitter/cmd/splitter@vX.Y.Z` records the
//     module version, which becomes the version;
//   - a build inside a git checkout records the revision (its first 12
//     characters become the commit, with a "-dirty" suffix when the working tree
//     had local modifications).
//
// Values set at link time are never overridden.
func versionFromBuildInfo(v, c string, info *debug.BuildInfo) (string, string) {
	if info == nil {
		return v, c
	}
	if v == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		v = info.Main.Version
	}
	if c == "none" {
		var rev string
		var dirty bool
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if rev != "" {
			c = rev[:min(12, len(rev))]
			if dirty {
				c += "-dirty"
			}
		}
	}
	return v, c
}

// resolveVersion applies versionFromBuildInfo to the package variables.
func resolveVersion() {
	info, _ := debug.ReadBuildInfo()
	version, commit = versionFromBuildInfo(version, commit, info)
}
