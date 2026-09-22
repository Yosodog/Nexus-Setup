//go:build !linux

package updater

import (
	"errors"
	"net"
)

type peerIdentity struct {
	UID uint32
	GID uint32
}

func unixPeerIdentity(net.Conn) (peerIdentity, error) {
	return peerIdentity{}, errors.New("peer credentials are only supported on Linux")
}
