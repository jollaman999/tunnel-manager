//go:build !dev

package install

// DevBuild says whether this is the development build, the one `make build`
// makes with -tags dev, which installs a release without checking its
// signature (signature_dev.go). This is every other build: `make release`, the
// container image and a plain `go build`, all of which check it.
const DevBuild = false
