package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/tunnel"
	"gorm.io/gorm"
)

// socksHeld narrows a read to the Hosts whose SOCKS5 proxy holds its port: the
// ones it is switched on for with a port to open. Whether the Host is enabled
// is not asked. A port given to a disabled Host is opened the moment the Host
// is enabled again, and a second row given the same port meanwhile would be
// found out then, by one of the two failing to start.
//
// socks_enabled is compared with true rather than read through COALESCE: a
// NULL left by AutoMigrate on a row from before the column existed is the
// proxy being off, and a NULL compares as nothing, which is what off is.
func socksHeld(db *gorm.DB) *gorm.DB {
	return db.Model(&models.Host{}).Where("socks_enabled = ? AND socks_port > 0", true)
}

// heldSocksPorts is the port of every SOCKS5 proxy socksHeld reads.
func heldSocksPorts(tx *gorm.DB) ([]int, error) {
	var ports []int

	err := socksHeld(tx).Pluck("socks_port", &ports).Error
	if err != nil {
		return nil, err
	}

	return ports, nil
}

// socksHolder is the Host whose SOCKS5 proxy opens port, other than the Host
// id. nil is none.
func socksHolder(tx *gorm.DB, port int, id uint) (*models.Host, error) {
	var holder models.Host

	err := socksHeld(tx).Where("socks_port = ? AND id <> ?", port, id).First(&holder).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return &holder, nil
}

// localForwardHolder is the local forward that opens port, and the Host it
// belongs to named the way the screens show it: by its address, or by its
// number when the Host is gone. The forward is nil when none opens it.
func localForwardHolder(tx *gorm.DB, port int) (*models.LocalForward, string, error) {
	var holder models.LocalForward

	err := tx.Where("local_port = ?", port).First(&holder).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}

	owner := strconv.FormatUint(uint64(holder.HostID), 10)

	var host models.Host
	if tx.First(&host, holder.HostID).Error == nil {
		owner = host.IP
	}

	return &holder, owner, nil
}

// checkSocks holds the SOCKS5 fields a Host is to be stored with to the rules
// the validator cannot say: a proxy that is switched on names its port, and
// the allowed sources are a list ParseAllowedSources reads. The scope and the
// range of the port are rules of the requests. A proxy that is off is held to
// the list all the same, since it is stored and switching the proxy on later
// is the one field that changes.
func checkSocks(host *models.Host) *refusal {
	if host.SocksEnabled && host.SocksPort <= 0 {
		return refuse(http.StatusBadRequest, errHostSocksPortRequired)
	}

	_, err := tunnel.ParseAllowedSources(host.SocksAllowedSources)
	if err != nil {
		return refuse(http.StatusBadRequest, errHostSocksSourcesInvalid, errorArgs{"reason": err.Error()})
	}

	return nil
}

// socksPortRefused checks the port of the SOCKS5 proxy of the Host id against
// what else on this machine it must not meet: the port of this server, stored
// or running, the proxy of another Host and a local forward. It is
// localPortRefused seen from the other side, and a non-nil error is a failed
// read in the same way.
func socksPortRefused(tx *gorm.DB, id uint, port int, apiPort int, runningPort int) (*refusal, error) {
	socksPort := strconv.Itoa(port)

	if isAPIPort(port, apiPort, runningPort) {
		return refuse(http.StatusConflict, errHostSocksPortIsAPIPort,
			errorArgs{"socks_port": socksPort}), nil
	}

	other, err := socksHolder(tx, port, id)
	if err != nil {
		return nil, err
	}
	if other != nil {
		return refuse(http.StatusConflict, errHostSocksPortTaken,
			errorArgs{"socks_port": socksPort, "host": other.IP}), nil
	}

	forward, owner, err := localForwardHolder(tx, port)
	if err != nil {
		return nil, err
	}
	if forward != nil {
		return refuse(http.StatusConflict, errHostSocksPortLocalForward,
			errorArgs{"socks_port": socksPort, "host": owner}), nil
	}

	return nil, nil
}
