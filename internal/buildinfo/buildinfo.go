// Package buildinfo reports the source revision the binary was built from.
package buildinfo

import "runtime/debug"

// revision is set at link time by image builds, whose build context has no
// .git directory:
//
//	-ldflags "-X go.kenn.io/benchdb/internal/buildinfo.revision=<commit>"
var revision string

// Revision returns the git commit the binary was built from, or "" when the
// build recorded none. A local `go build` in a checkout records it without a
// link flag.
func Revision() string {
	if revision != "" {
		return revision
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			return setting.Value
		}
	}
	return ""
}
