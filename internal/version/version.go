// Package version carries the build version injected at link time.
package version

// Version is set with -ldflags "-X .../internal/version.Version=...".
var Version = "dev"
