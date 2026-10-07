//go:build unix

package agentsvc

import "syscall"

// noFollow makes open fail on a link instead of following it.
const noFollow = syscall.O_NOFOLLOW
