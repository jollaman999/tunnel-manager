package api

import (
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/tunnel"
	"gorm.io/gorm"
)

// The two sorts of forward the status table carries. They are two because they
// run opposite ways: a service port tunnel opens a port on the Host and
// carries it to a service reached from here, and a local forward opens a port
// here and carries it to a target reached from the Host. A row says which one
// it is rather than leaving that to be guessed from the addresses, which look
// alike on both.
const (
	statusKindServicePort  = "service_port"
	statusKindLocalForward = "local_forward"
)

// statusConnected is the status a row of either sort carries while it is up:
// the word written on a tunnel row and the word a running local forward
// reports.
const statusConnected = "connected"

// statusRow is one row of the status table, either sort. The tunnel row is
// embedded so that a service port row carries exactly the fields it always
// carried, down to a column added to models.Tunnel later, and a local forward
// fills the few of them that mean something for it: status, last_error,
// retry_count and last_connected_at from what the forward reports, and server,
// local and remote from where it listens and what it reaches. The rest stay at
// their zero values, forward_reach among them, because nothing measures those
// for a local forward.
//
// local and remote are the mirror of what they hold on a tunnel row. On a
// tunnel, local is the port opened on the Host and remote the service reached
// from here; on a local forward, local is the port opened on this machine and
// remote the target reached from the Host. Which machine a row is talking
// about is what kind says.
type statusRow struct {
	models.Tunnel
	Kind string `json:"kind"`
	// SPID shadows Tunnel.SPID, which is a uint and cannot be empty: a local
	// forward is not carried by a service port and a zero there would read as
	// the service port numbered nothing. The pointer is null on those rows and
	// the number on the others. Both fields are tagged sp_id and this one is
	// the shallower, which is the one encoding/json writes.
	SPID *uint `json:"sp_id"`
}

// statusRef identifies one row of the status table in the order the table is
// paged in: the Host that carries it, which sort it is, and the id it has
// within that sort - the service port for a tunnel row and the row id for a
// local forward.
//
// It is read off the two tables on its own, without the rest of the row,
// because the page is a cut through both of them together and there is no way
// to know where that cut falls without having both lists in the one order.
type statusRef struct {
	HostID uint
	Kind   string
	Ref    uint
}

// statusKindOrder is where a sort stands within a Host: the service port
// tunnels first and then the local forwards. It is an order this program
// states rather than the alphabetical one of the words, which would put the
// local forwards first and would change under a sort renamed.
func statusKindOrder(kind string) int {
	if kind == statusKindLocalForward {
		return 1
	}

	return 0
}

// before reports whether this row stands ahead of the other one in the order
// the table is paged in. Host first, so that the rows of one Host are together
// on the page rather than the tunnels of every Host and then the forwards of
// every Host.
func (r statusRef) before(other statusRef) bool {
	if r.HostID != other.HostID {
		return r.HostID < other.HostID
	}

	if statusKindOrder(r.Kind) != statusKindOrder(other.Kind) {
		return statusKindOrder(r.Kind) < statusKindOrder(other.Kind)
	}

	return r.Ref < other.Ref
}

// statusTunnelRefs reads the reference of every tunnel row, in the order the
// tunnel table is read in. Only the two key columns are read: what the page
// needs from the rows that are not on it is where they stand.
func statusTunnelRefs(db *gorm.DB) ([]statusRef, error) {
	var refs []statusRef

	err := db.Model(&models.Tunnel{}).
		Select("host_id, sp_id AS ref").
		Order("host_id, sp_id").
		Scan(&refs).Error
	if err != nil {
		return nil, err
	}

	for i := range refs {
		refs[i].Kind = statusKindServicePort
	}

	return refs, nil
}

// statusLocalForwardRefs is statusTunnelRefs over the local forwards.
func statusLocalForwardRefs(db *gorm.DB) ([]statusRef, error) {
	var refs []statusRef

	err := db.Model(&models.LocalForward{}).
		Select("host_id, number AS ref").
		Order("host_id, number").
		Scan(&refs).Error
	if err != nil {
		return nil, err
	}

	for i := range refs {
		refs[i].Kind = statusKindLocalForward
	}

	return refs, nil
}

// mergeStatusRefs merges the two lists, each already in its own order, into the
// one order the table is paged in.
//
// The merge is done here rather than by a UNION in the database because the
// two lists are already sorted and walking them takes one pass with no SQL
// that either table would have to be described twice in. What is stored is a
// few dozen Hosts with their forwards, and the pool holds one connection
// (internal/database, SetMaxOpenConns(1)), so a query saved is a turn of that
// connection saved.
func mergeStatusRefs(tunnels, forwards []statusRef) []statusRef {
	merged := make([]statusRef, 0, len(tunnels)+len(forwards))

	t, f := 0, 0
	for t < len(tunnels) && f < len(forwards) {
		if forwards[f].before(tunnels[t]) {
			merged = append(merged, forwards[f])
			f++

			continue
		}

		merged = append(merged, tunnels[t])
		t++
	}

	merged = append(merged, tunnels[t:]...)
	merged = append(merged, forwards[f:]...)

	return merged
}

// statusPageSplit is where the page falls on each of the two tables: how many
// rows of that sort stand before the page, which is the offset the rows are
// read from, and how many of them are on it.
type statusPageSplit struct {
	tunnelOffset  int
	tunnelCount   int
	forwardOffset int
	forwardCount  int
}

// splitStatusPage counts the merged references into that split. The rows of a
// page are a run of the merged order, so the rows of one sort on it are a run
// of that sort's own order too, which is what lets each table be read with the
// LIMIT and OFFSET it is already read with rather than by a list of ids.
func splitStatusPage(refs []statusRef, offset, size int) statusPageSplit {
	var split statusPageSplit

	for i, ref := range refs {
		if i >= offset+size {
			break
		}

		onPage := i >= offset

		switch {
		case ref.Kind == statusKindLocalForward && onPage:
			split.forwardCount++
		case ref.Kind == statusKindLocalForward:
			split.forwardOffset++
		case onPage:
			split.tunnelCount++
		default:
			split.tunnelOffset++
		}
	}

	return split
}

// statusPageRow is one row of the page beside the reference it is ordered by.
// The reference is taken from the row itself rather than from the list the
// page was cut with, so that the order of the answer holds even if a row was
// written between the two reads.
type statusPageRow struct {
	ref statusRef
	row statusRow
}

// tunnelStatusRow is one tunnel row as the status table carries it.
func tunnelStatusRow(t models.Tunnel) statusPageRow {
	spID := t.SPID

	return statusPageRow{
		ref: statusRef{HostID: t.HostID, Kind: statusKindServicePort, Ref: t.SPID},
		row: statusRow{Tunnel: t, Kind: statusKindServicePort, SPID: &spID},
	}
}

// localForwardStatusRow is one local forward as the status table carries it:
// what localForwardViewOf says about it, which is the one place that decides
// what a forward reports, on the addresses tunnel.LocalForwardAddresses builds,
// which is the one place that decides where it listens.
//
// host is nil when the row names a Host that is not there. Such a row runs
// nothing, which is what a disabled Host reports, and it has no server address
// to name; GetLocalForward answers one the same way.
//
// The listen address is the IPv4 one of the pair, for the reason the tunnel
// rows carry that one (see models.Tunnel.Local): a scope names two addresses
// and the row carries the one to connect to.
func localForwardStatusRow(lf models.LocalForward, host *models.Host,
	states map[uint]tunnel.LocalForwardState) statusPageRow {
	owner := models.Host{}
	if host != nil {
		owner = *host
	}

	listenV4, _, server, target := tunnel.LocalForwardAddresses(&owner, &lf)
	if host == nil {
		server = ""
	}

	view := localForwardViewOf(lf, host != nil && host.Enabled, states)

	return statusPageRow{
		ref: statusRef{HostID: lf.HostID, Kind: statusKindLocalForward, Ref: lf.Number},
		row: statusRow{
			Tunnel: models.Tunnel{
				HostID:          lf.HostID,
				Status:          view.Status,
				LastError:       view.LastError,
				RetryCount:      view.RetryCount,
				LastConnectedAt: view.LastConnectedAt,
				Server:          server,
				Local:           listenV4,
				Remote:          target,
			},
			Kind: statusKindLocalForward,
		},
	}
}

// mergeStatusRows merges the rows read from the two tables into the order the
// references were paged in, and hands back the rows alone.
func mergeStatusRows(tunnels, forwards []statusPageRow) []statusRow {
	rows := make([]statusRow, 0, len(tunnels)+len(forwards))

	t, f := 0, 0
	for t < len(tunnels) && f < len(forwards) {
		if forwards[f].ref.before(tunnels[t].ref) {
			rows = append(rows, forwards[f].row)
			f++

			continue
		}

		rows = append(rows, tunnels[t].row)
		t++
	}

	for _, left := range tunnels[t:] {
		rows = append(rows, left.row)
	}

	for _, left := range forwards[f:] {
		rows = append(rows, left.row)
	}

	return rows
}

// connectedLocalForwardCount is how many local forwards report that they are
// connected. It is counted from what the running forwards say rather than from
// the rows, because that is where the status of a local forward lives: there
// is no column to count, and the rows of forwards that are switched off or
// carried by a disabled Host report nothing at all.
func connectedLocalForwardCount(states map[uint]tunnel.LocalForwardState) int {
	connected := 0

	for _, state := range states {
		if state.Status == statusConnected {
			connected++
		}
	}

	return connected
}
