//go:build linux

package updater

import (
	"fmt"
	"net"
	"syscall"
)

type peerIdentity struct {
	UID uint32
	GID uint32
}

func unixPeerIdentity(connection net.Conn) (peerIdentity, error) {
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return peerIdentity{}, fmt.Errorf("connection is not a Unix socket")
	}
	var identity peerIdentity
	var controlErr error
	rawConnection, rawErr := unixConnection.SyscallConn()
	if rawErr != nil {
		return peerIdentity{}, rawErr
	}
	err := rawConnection.Control(func(fileDescriptor uintptr) {
		credentials, credentialErr := syscall.GetsockoptUcred(int(fileDescriptor), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if credentialErr != nil {
			controlErr = credentialErr
			return
		}
		identity.UID = credentials.Uid
		identity.GID = credentials.Gid
	})
	if err != nil {
		return peerIdentity{}, err
	}
	if controlErr != nil {
		return peerIdentity{}, controlErr
	}
	return identity, nil
}
