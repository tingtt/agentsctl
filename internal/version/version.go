// Package version holds the agentsctl application release version.
//
// Version is the single source of truth for the running release. It is set at
// link time (see .github/workflows/release.yml and internal/selfupdate's
// install command):
//
//	-ldflags="-X github.com/tingtt/agentsctl/internal/version.Version=v1.2.3"
//
// A build without that flag keeps the development default.
package version

// Development is the Version of a build that did not inject a release
// version. Such a build never checks for updates.
const Development = "dev"

// Version is the agentsctl release version, injected with -ldflags -X.
var Version = Development
