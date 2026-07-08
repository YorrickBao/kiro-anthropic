package main

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"syscall"
)

// listenWithAutoIncrement binds a TCP listener on host:port, incrementing the
// port by one and retrying whenever the address is already in use, until it
// finds a free port or exhausts the range up to 65535. It returns the listener
// and the port actually bound. Errors other than "address in use" are returned
// immediately.
func listenWithAutoIncrement(host string, port int) (net.Listener, int, error) {
	if port < 1 || port > 65535 {
		return nil, 0, fmt.Errorf("invalid start port %d", port)
	}
	for p := port; p <= 65535; p++ {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(p)))
		if err == nil {
			return ln, p, nil
		}
		if !isAddrInUse(err) {
			return nil, 0, err
		}
	}
	return nil, 0, fmt.Errorf("no free port available from %d to 65535 on %s", port, host)
}

// isAddrInUse reports whether err is an "address already in use" bind failure.
func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}
