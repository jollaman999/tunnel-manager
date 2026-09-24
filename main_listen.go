package main

import (
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
)

// apiListenAttempts is how many ports the system is asked for when the stored
// API port is taken and the ones it hands back keep landing on a port avoid
// refuses.
const apiListenAttempts = 20

// previousAPIPortEnv carries the port the API was served on across a restart.
// It is for this program only: the process that runs this program again sets
// it, and the image that replaces it reads it once and takes it out of its own
// environment.
const previousAPIPortEnv = "TUNNEL_MANAGER_PREVIOUS_API_PORT"

// listenAPI opens the port the API is served on and reports which one it is.
//
// The stored port is tried first. Only when another program holds it is
// another port tried instead: the screen that would change the stored port is
// served on this port, so a process that ended here would leave no way to
// change it. Any other failure, a port below 1024 without the privilege for it
// among them, is returned as it is, because another port would hide a setting
// that has to be fixed.
//
// previous is the port the process that ran before a restart was served on, or
// 0 when there was none. It is tried before the system is asked for a port,
// because the screen that asked for the restart looks for the service there
// first. A previous port avoid refuses is not tried, and one that cannot be
// opened is passed over without a word, since the system still has a port to
// hand out.
//
// avoid refuses a port the system picked. A refused port is held open until a
// port is settled on, so that the system does not hand the same one back. When
// no attempt lands on a port avoid accepts, the error of the stored port is
// returned.
func listenAPI(host string, stored, previous int, avoid func(port int) bool) (net.Listener, int, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(stored)))
	if err == nil {
		return listener, stored, nil
	}

	if !addressInUse(err) {
		return nil, stored, err
	}

	if previous > 0 && previous != stored && !avoid(previous) {
		again, againErr := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(previous)))
		if againErr == nil {
			return again, previous, nil
		}
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

// takePreviousAPIPort reads the port a restart handed over and takes it out of
// the environment of this process, so that a later restart hands over the port
// it is on then and never this one again. A value that is not a port is read
// as none.
func takePreviousAPIPort() int {
	value, ok := os.LookupEnv(previousAPIPortEnv)
	if !ok {
		return 0
	}

	_ = os.Unsetenv(previousAPIPortEnv)

	port, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || port < 1 || port > 65535 {
		return 0
	}

	return port
}

// restartEnviron is the environment a restart hands to the image that
// replaces this process: this one with the port the API is served on in
// previousAPIPortEnv, in place of any value it held already. A port of 0
// hands over none.
func restartEnviron(port int) []string {
	env := make([]string, 0, len(os.Environ())+1)

	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, previousAPIPortEnv+"=") {
			continue
		}

		env = append(env, kv)
	}

	if port > 0 {
		env = append(env, previousAPIPortEnv+"="+strconv.Itoa(port))
	}

	return env
}
