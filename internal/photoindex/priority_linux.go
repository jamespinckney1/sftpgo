// Copyright (C) 2019 Nicola Murino
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, version 3.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.
//go:build linux

package photoindex

import "golang.org/x/sys/unix"

// lowerPriority lowers the CPU and I/O scheduling priority of a helper process
// (exiftool, vipsthumbnail) so background indexing never competes with file
// transfers. Errors are ignored: this is best effort.
func lowerPriority(pid int) {
	unix.Setpriority(unix.PRIO_PROCESS, pid, 10) //nolint:errcheck
	// ioprio_set(IOPRIO_WHO_PROCESS, pid, IOPRIO_CLASS_IDLE): only use the disk
	// when nobody else needs it.
	const (
		ioprioWhoProcess = 1
		ioprioClassIdle  = 3
		ioprioClassShift = 13
	)
	unix.Syscall(unix.SYS_IOPRIO_SET, ioprioWhoProcess, uintptr(pid), ioprioClassIdle<<ioprioClassShift) //nolint:errcheck
}
