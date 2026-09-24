package main

import (
	"errors"
	"net"
	"strconv"
)

// apiListenAttempts is how many ports the system is asked for when the stored
// API port is taken and the ones it hands back keep landing on a port avoid
// refuses.
const apiListenAttempts = 20

// listenAPI opens the port the API is served on and reports which one it is.
//
// The stored port is tried first. Only when another program holds it is the
// system asked for a free one instead: the screen that would change the stored
// port is served on this port, so a process that ended here would leave no way
// to change it. Any other failure, a port below 1024 without the privilege for
// it among them, is returned as it is, because another port would hide a
// setting that has to be fixed.
//
// avoid refuses a port the system picked. A refused port is held open until a
// port is settled on, so that the system does not hand the same one back. When
// no attempt lands on a port avoid accepts, the error of the stored port is
// returned.
func listenAPI(host string, stored int, avoid func(port int) bool) (net.Listener, int, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(stored)))
	if err == nil {
		return listener, stored, nil
	}

	if !addressInUse(err) {
		return nil, stored, err
	}

	var refused []net.Listener

	defer func() {
		for _, r := range refused {
			_ = r.Close()
		}
	}()

	for range apiListenAttempts {
		other, otherErr := net.Listen("tcp", net.JoinHostPort(host, "0"))
		if otherErr != nil {
			return nil, stored, errors.Join(err, otherErr)
		}

		port := other.Addr().(*net.TCPAddr).Port
		if !avoid(port) {
			return other, port, nil
		}

		refused = append(refused, other)
	}

	return nil, stored, err
}
