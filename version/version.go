package version

import (
	"fmt"
	"runtime"
)

var (
	Version   = "0.8.0"
	GitCommit = "dev"
	BuildTime = "unknown"
	GoVersion = runtime.Version()
)

// Full returns a formatted multi-line version and build info string.
func Full() string {
	return fmt.Sprintf("spp version %s\ngit commit: %s\nbuild time: %s\ngo version: %s\nos/arch:    %s/%s",
		Version, GitCommit, BuildTime, GoVersion, runtime.GOOS, runtime.GOARCH)
}

// Short returns a concise single-line version string.
func Short() string {
	return fmt.Sprintf("spp %s (%s, %s/%s)", Version, GitCommit, runtime.GOOS, runtime.GOARCH)
}
