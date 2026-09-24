package tunnel

import (
	"fmt"
	"net"
)

// openEachAddress opens the local port on both addresses of the pair, each
// with the network of its own family, and reports which of them it did. One of
// them opening is enough, and neither opening is the error, which names both
// refusals.
func openEachAddress(pair localPair, listen func(network string, address *net.TCPAddr) (net.Listener, error)) (*openForwards, error) {
	listenerV4, errV4 := listen("tcp4", pair.v4)
	listenerV6, errV6 := listen("tcp6", pair.v6)

	switch {
	case errV4 != nil && errV6 != nil:
		return nil, fmt.Errorf("neither address of the bind scope could be opened on this machine (%s: %v; %s: %v)",
			pair.v4, errV4, pair.v6, errV6)
	case errV6 != nil:
		return &openForwards{v4: listenerV4, reach: openReachV4, refused: errV6}, nil
	case errV4 != nil:
		return &openForwards{v6: listenerV6, reach: openReachV6, refused: errV4}, nil
	}

	return &openForwards{v4: listenerV4, v6: listenerV6, reach: openReachBoth}, nil
}
