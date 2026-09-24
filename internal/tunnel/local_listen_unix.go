//go:build !windows

package tunnel

import "net"

// openLocalListeners opens the local port on both addresses of the pair and
// reports which of them it did.
//
// Each address is opened with the network of its own family. On Linux a plain
// "tcp" listener on [::] also takes the IPv4 wildcard, so asking for 0.0.0.0
// and [::] that way fails the second with "address already in use". tcp6 sets
// IPV6_V6ONLY and leaves IPv4 to the tcp4 listener, which makes the pair two
// listeners that do not overlap, and lets a machine without IPv6 open the IPv4
// half alone.
func openLocalListeners(pair localPair) (*openForwards, error) {
	return openEachAddress(pair, func(network string, address *net.TCPAddr) (net.Listener, error) {
		listener, err := net.ListenTCP(network, address)
		if err != nil {
			return nil, err
		}

		return listener, nil
	})
}
