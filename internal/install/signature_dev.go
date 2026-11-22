//go:build dev

package install

// DevBuild says whether this is the development build, and this file is only
// compiled into that one, which `make build` makes with -tags dev.
//
// A development build installs a release on its checksum alone and does not
// check the signature over SHA256SUMS. It is what somebody builds to work on
// the program, on a machine that may hold none of the release key, and a build
// that refused every release it could not check would leave the update path
// unusable there. It is not a build to run anywhere a release is trusted: the
// checksum only says the binary agrees with SHA256SUMS, and whoever can change
// one file of a release can change both. The process says so in its log as it
// starts and the Update screen says so beside the version.
const DevBuild = true
