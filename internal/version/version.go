// Package version holds build information.
package version

// Version is overridden at build time with -ldflags "-X ...version.Version=v1.2.3".
var Version = "0.1.0-dev"
