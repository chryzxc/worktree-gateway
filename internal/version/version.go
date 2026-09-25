// Package version holds build information.
package version

import "runtime/debug"

// Version is set at build time with -ldflags "-X ...version.Version=v1.2.3".
// Builds without it (`go install ...@v1.2.3`) fall back to the module
// version recorded by the Go toolchain.
var Version = ""

func init() {
	if Version != "" {
		return
	}
	Version = "dev"
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		Version = bi.Main.Version
	}
}
