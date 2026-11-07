//go:build !linux && !darwin

package frame

import (
	"errors"
	"net"
)

func setNotSentLowat(*net.TCPConn, int) error {
	return errors.New("TCP_NOTSENT_LOWAT unsupported on this platform")
}
