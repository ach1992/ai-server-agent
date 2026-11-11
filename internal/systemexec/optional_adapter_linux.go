package systemexec

import "golang.org/x/sys/unix"

func renameAdapterProfilesExchange(active, previous string) error {
	return unix.Renameat2(unix.AT_FDCWD, active, unix.AT_FDCWD, previous, unix.RENAME_EXCHANGE)
}
