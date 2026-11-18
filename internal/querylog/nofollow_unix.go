//go:build unix

package querylog

import "syscall"

// oNoFollow makes opening a segment fail when the name is a symlink.
const oNoFollow = syscall.O_NOFOLLOW
