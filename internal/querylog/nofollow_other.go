//go:build !unix

package querylog

// oNoFollow is zero where the platform has no O_NOFOLLOW; segments are then
// opened without the symlink check.
const oNoFollow = 0
