//go:build linux

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
	var credential *unix.Ucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credential, socketErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return processinfo.Identity{}, err
	}
	if socketErr != nil {
		return processinfo.Identity{}, socketErr
	}
	identity, err := processinfo.Observe(int(credential.Pid))
	if err != nil {
		return processinfo.Identity{}, err
	}
	if identity.UID != credential.Uid {
		return processinfo.Identity{}, errors.New("supervisor peer credential changed")
	}
	return identity, nil
}
