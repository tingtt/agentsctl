//go:build darwin

package supervisor

import (
	"errors"
	"net"

	processinfo "github.com/tingtt/agentsctl/internal/process"
	"golang.org/x/sys/unix"
)

func peerIdentity(conn net.Conn) (processinfo.Identity, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return processinfo.Identity{}, errors.New("supervisor connection is not a Unix socket")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return processinfo.Identity{}, err
	}
	var pid int
	var uid uint32
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		pid, socketErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
		if socketErr != nil {
			return
		}
		credential, credentialErr := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if credentialErr != nil {
			socketErr = credentialErr
			return
		}
		uid = credential.Uid
	}); err != nil {
		return processinfo.Identity{}, err
	}
	if socketErr != nil {
		return processinfo.Identity{}, socketErr
	}
	identity, err := processinfo.Observe(pid)
	if err != nil {
		return processinfo.Identity{}, err
	}
	if identity.UID != uid {
		return processinfo.Identity{}, errors.New("supervisor peer credential changed")
	}
	return identity, nil
}
