package api

import (
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/jollaman999/tunnel-manager/internal/models"
	"gorm.io/gorm"
)

// maxJumpHosts is how many Hosts a jump route may go through. Every one of
// them is a connection made under its own time limit, so a route of many is a
// Host that takes many of those limits to be reached or to be found out of
// reach.
const maxJumpHosts = 8

// jumpRoutesOf reads the jump route of each of hostIDs, in the order the route
// goes, keyed by Host. It is one query for the whole set, so that a page of
// Hosts costs one read rather than one per row. A Host without a route has no
// entry.
func jumpRoutesOf(db *gorm.DB, hostIDs []uint) (map[uint][]uint, error) {
	routes := make(map[uint][]uint)
	if len(hostIDs) == 0 {
		return routes, nil
	}

	var rows []models.HostJump

	err := db.Where("host_id IN ?", hostIDs).Order("host_id, seq").Find(&rows).Error
	if err != nil {
		return nil, err
	}

	for _, row := range rows {
		routes[row.HostID] = append(routes[row.HostID], row.JumpHostID)
	}

	return routes, nil
}

// jumpRouteOf is jumpRoutesOf for one Host. A Host without a route answers an
// empty slice.
func jumpRouteOf(db *gorm.DB, hostID uint) ([]uint, error) {
	routes, err := jumpRoutesOf(db, []uint{hostID})
	if err != nil {
		return nil, err
	}

	route := routes[hostID]
	if route == nil {
		route = []uint{}
	}

	return route, nil
}

// jumpRouteRefused checks a route a request asked for. self is the Host the
// route belongs to, and zero for one that is being registered and so has no
// number that could be on the route. A disabled Host is taken as a step: the
// route is kept and waits for it to be enabled again.
//
// A non-nil error is a failed read, as it is for socksPortRefused.
func jumpRouteRefused(tx *gorm.DB, self uint, route []uint) (*refusal, error) {
	if len(route) > maxJumpHosts {
		return refuse(http.StatusBadRequest, errHostJumpTooMany,
			errorArgs{"max": strconv.Itoa(maxJumpHosts)}), nil
	}

	seen := make(map[uint]bool, len(route))

	for _, id := range route {
		if self != 0 && id == self {
			return refuse(http.StatusBadRequest, errHostJumpSelf), nil
		}
		if seen[id] {
			return refuse(http.StatusBadRequest, errHostJumpRepeated,
				errorArgs{"host_id": strconv.FormatUint(uint64(id), 10)}), nil
		}
		seen[id] = true
	}

	if len(route) == 0 {
		return nil, nil
	}

	var stored []uint

	err := tx.Model(&models.Host{}).Where("id IN ?", route).Pluck("id", &stored).Error
	if err != nil {
		return nil, err
	}

	found := make(map[uint]bool, len(stored))
	for _, id := range stored {
		found[id] = true
	}

	for _, id := range route {
		if !found[id] {
			return refuse(http.StatusBadRequest, errHostJumpUnknown,
				errorArgs{"host_id": strconv.FormatUint(uint64(id), 10)}), nil
		}
	}

	return nil, nil
}

// writeJumpRoute replaces the route of hostID with route, numbered from 1 in
// the order it was given. It runs in the transaction that writes the Host, so
// that the Host and its route land together.
func writeJumpRoute(tx *gorm.DB, hostID uint, route []uint) error {
	err := tx.Where("host_id = ?", hostID).Delete(&models.HostJump{}).Error
	if err != nil {
		return err
	}

	for i, jumpID := range route {
		err = tx.Create(&models.HostJump{HostID: hostID, Seq: uint(i + 1), JumpHostID: jumpID}).Error
		if err != nil {
			return err
		}
	}

	return nil
}

// jumpUser is a Host whose route goes through the Host a delete was asked for,
// as the refusal of that delete names it.
type jumpUser struct {
	ID      uint   `json:"id"`
	Address string `json:"address"`
	Port    int    `json:"port"`
}

// jumpUsersOf is every Host whose route goes through hostID, oldest first.
func jumpUsersOf(tx *gorm.DB, hostID uint) ([]jumpUser, error) {
	var users []jumpUser

	err := tx.Model(&models.Host{}).Distinct("hosts.id", "hosts.address", "hosts.port").
		Joins("JOIN host_jumps ON host_jumps.host_id = hosts.id").
		Where("host_jumps.jump_host_id = ?", hostID).
		Order("hosts.id").Find(&users).Error
	if err != nil {
		return nil, err
	}

	return users, nil
}

// jumpUsersRefusal is the refusal of deleting a Host that users go through.
// The Hosts are named in the sentence by address and handed over whole beside
// it, so that a screen can point at the rows.
func jumpUsersRefusal(users []jumpUser) *refusal {
	ids := make([]string, 0, len(users))
	addresses := make([]string, 0, len(users))

	for _, user := range users {
		ids = append(ids, strconv.FormatUint(uint64(user.ID), 10))
		addresses = append(addresses, net.JoinHostPort(user.Address, strconv.Itoa(user.Port)))
	}

	return refuse(http.StatusConflict, errHostDeleteUsedAsJump,
		errorArgs{"host_ids": strings.Join(ids, ","), "hosts": strings.Join(addresses, ", ")}).
		carrying(users)
}
