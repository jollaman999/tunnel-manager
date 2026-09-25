package api

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/settings"
	"github.com/jollaman999/tunnel-manager/internal/tunnel"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// The three statuses a row carries when no forward runs for it. The manager
// reports nothing for such a row, and the reasons are kept apart because they
// leave the operator with different work: a disabled Host is switched on, a
// forward that is off is switched on on its own row, and a forward that is on
// and not running under an enabled Host is either waiting for the next
// reconcile pass or failed to start, which the log says.
const (
	localForwardStatusDisabled = "disabled"
	localForwardStatusOff      = "off"
	localForwardStatusStopped  = "stopped"
)

// localForwardView is one local forward as an answer carries it: the row, with
// what the running forward reports laid over it. The state is spelled out
// rather than embedded as tunnel.LocalForwardState so that the description
// generated from these types does not have to reach into package tunnel.
type localForwardView struct {
	models.LocalForward
	// Status is what tunnel.LocalForwardState reports while a forward runs,
	// and "disabled", "off" or "stopped" while none does.
	Status          string    `json:"status"`
	LastError       string    `json:"last_error"`
	RetryCount      int       `json:"retry_count"`
	LastConnectedAt time.Time `json:"last_connected_at"`
}

// localForwardViewOf builds the view of one row. The scope goes out as a word
// every time, since an empty one is the wildcard and a screen that had to know
// that would be a second place keeping the rule.
func localForwardViewOf(lf models.LocalForward, hostEnabled bool,
	states map[tunnel.LocalForwardKey]tunnel.LocalForwardState) localForwardView {
	if lf.BindScope == "" {
		lf.BindScope = models.BindScopeWildcard
	}

	view := localForwardView{LocalForward: lf}

	state, running := states[tunnel.LocalForwardKey{HostID: lf.HostID, Number: lf.Number}]
	switch {
	case !hostEnabled:
		view.Status = localForwardStatusDisabled
	case !lf.Enabled:
		view.Status = localForwardStatusOff
	case !running:
		view.Status = localForwardStatusStopped
	default:
		view.Status = state.Status
		view.LastError = state.LastError
		view.RetryCount = state.RetryCount
		view.LastConnectedAt = state.LastConnectedAt
	}

	return view
}

// storedAPIPort reads the port this server is stored to listen on. It is read
// before the transaction is opened: the pool holds one connection, and a read
// through h.db inside the transaction would wait on it forever.
//
// The stored port is checked as well as the one this process came up on
// (runningAPIPort), because a forward is kept for longer than this process runs
// and the stored port is the one every later startup listens on.
func (h *Handler) storedAPIPort() (int, error) {
	stored, err := settings.Load(h.db)
	if err != nil {
		return 0, err
	}

	return stored.APIPort, nil
}

// isAPIPort reports whether port is the stored api_port or the port this
// process listens on. runningPort is 0 when that port is not known, and then
// only the stored one is held against port.
func isAPIPort(port int, storedPort int, runningPort int) bool {
	return port == storedPort || (runningPort != 0 && port == runningPort)
}

// localPortRefused checks the local port of a forward against the three things
// on this machine it must not meet: the port of this server, stored or running,
// the port of another forward and the port of a SOCKS5 proxy. hostID and
// number are the row being changed, or both zero for a new one, which is a row
// no stored forward can be. A non-nil refusal is the answer; a non-nil error is a failed read,
// which the caller answers with the failure of its own write.
func localPortRefused(tx *gorm.DB, hostID uint, number uint, localPort int, apiPort int, runningPort int) (*refusal, error) {
	if isAPIPort(localPort, apiPort, runningPort) {
		return refuse(http.StatusConflict, errLocalForwardPortIsAPIPort,
			errorArgs{"local_port": strconv.Itoa(localPort)}), nil
	}

	var taken int64

	err := tx.Model(&models.LocalForward{}).
		Where("local_port = ? AND NOT (host_id = ? AND number = ?)", localPort, hostID, number).
		Count(&taken).Error
	if err != nil {
		return nil, err
	}

	if taken > 0 {
		return refuse(http.StatusConflict, errLocalForwardPortTaken,
			errorArgs{"local_port": strconv.Itoa(localPort)}), nil
	}

	proxy, err := socksHolder(tx, localPort, 0)
	if err != nil {
		return nil, err
	}

	if proxy != nil {
		return refuse(http.StatusConflict, errLocalForwardPortSocks,
			errorArgs{"local_port": strconv.Itoa(localPort), "host": proxy.Address}), nil
	}

	return nil, nil
}

// checkLocalForwardSources holds the allowed sources a request carries to the
// list ParseAllowedSources reads, as checkSocks holds those of a proxy. A list
// is taken on either scope, as the proxy takes one: on the loopback it is held
// to the addresses of this machine alone, which is the operator's choice to
// make and not one to refuse.
func checkLocalForwardSources(sources *string) *refusal {
	if sources == nil {
		return nil
	}

	_, err := tunnel.ParseAllowedSources(*sources)
	if err != nil {
		return refuse(http.StatusBadRequest, errLocalForwardSourcesBad, errorArgs{"reason": err.Error()})
	}

	return nil
}

// nextLocalForwardNumber is the number a forward added to this Host is given:
// the lowest one the Host is not already using.
//
// The gaps are filled rather than left, so the numbers of a Host are 1, 2, 3
// with nothing missing and the one that was deleted comes back on the next
// forward added. What that costs is that a number written down somewhere else,
// in a log line or in a note, names the row that holds it now rather than the
// row it was written about.
//
// The numbers are read and walked here instead of being worked out in SQL. A
// Host carries a handful of forwards, the primary key makes them unique and the
// order makes them ascending, so the first place the run breaks is the answer.
func nextLocalForwardNumber(tx *gorm.DB, hostID uint) (uint, error) {
	var taken []uint

	err := tx.Model(&models.LocalForward{}).Where("host_id = ?", hostID).
		Order("number").Pluck("number", &taken).Error
	if err != nil {
		return 0, err
	}

	next := uint(1)

	for _, number := range taken {
		if number != next {
			break
		}

		next++
	}

	return next, nil
}

// localForwardPath reads the Host and the number the path of one forward
// names. The three handlers that answer under such a path read them here, so
// the two refusals a path that names neither of them with a number is answered
// with are raised in one place.
func localForwardPath(c echo.Context) (uint, uint, *refusal) {
	hostID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		return 0, 0, refuse(http.StatusBadRequest, errHostIDInvalid, errorArgs{"reason": err.Error()})
	}

	number, err := strconv.ParseUint(c.Param("number"), 10, 32)
	if err != nil {
		return 0, 0, refuse(http.StatusBadRequest, errLocalForwardNumberInvalid, errorArgs{"reason": err.Error()})
	}

	return uint(hostID), uint(number), nil
}

// apiPortHolder is the local forward that opens the port a change of the
// settings asked to store as api_port, as the refusal of that change carries it.
type apiPortHolder struct {
	// Number is which forward of its Host this is. It is the number and not a
	// table-wide id because that is what the row is keyed by, and it is read
	// beside HostID: a number on its own names a row on every Host.
	Number        uint   `json:"number"`
	HostID        uint   `json:"host_id"`
	HostAddress   string `json:"host_address"`
	LocalPort     int    `json:"local_port"`
	TargetAddress string `json:"target_address"`
	TargetPort    int    `json:"target_port"`
}

// apiPortSocksHolder is the Host whose SOCKS5 proxy opens that port.
type apiPortSocksHolder struct {
	HostID      uint   `json:"host_id"`
	HostAddress string `json:"host_address"`
	SocksPort   int    `json:"socks_port"`
}

// apiPortTaken is the data of that refusal: what is in the way, and a port
// either of the two could move to. SuggestedPort is 0 when no port is free.
//
// What is in the way is a local forward or the SOCKS5 proxy of a Host. The
// first is in LocalForward, as it always was. The second is in SocksHost, and
// LocalForward is then left at its zero value, so that a client reading the
// shape from before the proxies still finds the object it reads.
type apiPortTaken struct {
	LocalForward  apiPortHolder       `json:"local_forward"`
	SocksHost     *apiPortSocksHolder `json:"socks_host,omitempty"`
	SuggestedPort int                 `json:"suggested_port"`
}

// apiPortCodes are the refusals a caller of apiPortRefused answers under: one
// for a local forward in the way and one for a SOCKS5 proxy.
type apiPortCodes struct {
	forward errorCode
	socks   errorCode
}

// apiPortRefused checks a port asked for as api_port against the local ports
// of the forwards and the ports of the SOCKS5 proxies. codes are the refusals
// the caller answers under, storedPort is the api_port stored now and
// runningPort the port this process listens on, which the suggested port stays
// clear of as well. Like localPortRefused, a non-nil error is a failed read.
func apiPortRefused(tx *gorm.DB, codes apiPortCodes, apiPort int, storedPort int, runningPort int) (*refusal, error) {
	var holder models.LocalForward

	err := tx.Where("local_port = ?", apiPort).First(&holder).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return apiPortSocksRefused(tx, codes.socks, apiPort, storedPort, runningPort)
	}
	if err != nil {
		return nil, err
	}

	// A row whose Host is gone is named by the number, as an import names it.
	hostIP := ""
	host := strconv.FormatUint(uint64(holder.HostID), 10)

	var owner models.Host
	if tx.First(&owner, holder.HostID).Error == nil {
		hostIP = owner.Address
		host = owner.Address
	}

	suggested, err := freeLocalPort(tx, apiPort, storedPort, runningPort)
	if err != nil {
		return nil, err
	}

	return refuse(http.StatusConflict, codes.forward, errorArgs{
		"api_port": strconv.Itoa(apiPort),
		"host":     host,
		"target":   net.JoinHostPort(holder.TargetAddress, strconv.Itoa(holder.TargetPort)),
	}).carrying(apiPortTaken{
		LocalForward: apiPortHolder{
			Number:        holder.Number,
			HostID:        holder.HostID,
			HostAddress:   hostIP,
			LocalPort:     holder.LocalPort,
			TargetAddress: holder.TargetAddress,
			TargetPort:    holder.TargetPort,
		},
		SuggestedPort: suggested,
	}), nil
}

// apiPortSocksRefused is the half of apiPortRefused that looks at the SOCKS5
// proxies.
func apiPortSocksRefused(tx *gorm.DB, code errorCode, apiPort int, storedPort int, runningPort int) (*refusal, error) {
	holder, err := socksHolder(tx, apiPort, 0)
	if err != nil || holder == nil {
		return nil, err
	}

	suggested, err := freeLocalPort(tx, apiPort, storedPort, runningPort)
	if err != nil {
		return nil, err
	}

	return refuse(http.StatusConflict, code, errorArgs{
		"api_port": strconv.Itoa(apiPort),
		"host":     holder.Address,
	}).carrying(apiPortTaken{
		SocksHost: &apiPortSocksHolder{
			HostID:      holder.ID,
			HostAddress: holder.Address,
			SocksPort:   holder.SocksPort,
		},
		SuggestedPort: suggested,
	}), nil
}

// freeLocalPort is the first port after taken that no local forward and no
// SOCKS5 proxy opens and that is none of taken, storedPort and runningPort. It
// goes up to 65535 and goes on from 1024, below which a port is one the
// operator picks on purpose; 0 is none.
func freeLocalPort(tx *gorm.DB, taken int, storedPort int, runningPort int) (int, error) {
	var ports []int

	err := tx.Model(&models.LocalForward{}).Pluck("local_port", &ports).Error
	if err != nil {
		return 0, err
	}

	proxies, err := heldSocksPorts(tx)
	if err != nil {
		return 0, err
	}

	ports = append(ports, proxies...)

	held := map[int]bool{taken: true, storedPort: true, runningPort: true}
	for _, port := range ports {
		held[port] = true
	}

	port := taken
	for range 65535 {
		port++
		if port > 65535 {
			port = 1024
		}

		if !held[port] {
			return port, nil
		}
	}

	return 0, nil
}

// ListHostLocalForwards answers one page of the local forwards of one Host,
// ordered by id for the reason ListHosts is, in the shape every other list is
// answered in.
//
// @Summary      One page of the local forwards of a Host, oldest first, with what each one reports
// @Description  status is what the running forward reports (starting, connected, reconnecting, error, host_key_unapproved, host_key_mismatch), "disabled" when the Host is disabled, "off" when the Host is enabled and the forward is switched off, and "stopped" when both are on and no forward runs yet or it failed to start.
// @Description  bind_scope is always loopback or wildcard.
// @Tags         local forwards
// @Produce  json
// @Param   id    path   int  true   "The id of the Host"
// @Param   page  query  int  false  "The page, counted from 1. Below 1 is read as 1, and a page past the last one is answered with the last page"
// @Param   size  query  int  false  "How many rows a page holds"  Enums(10, 20, 30, 50, 100)
// @Success  200  {object}  models.Response{data=api.listPageOf{items=[]api.localForwardView}}
// @Failure  400  {object}  api.errorBody  "page is not a number, or size is not one of the sizes taken"
// @Failure  404  {object}  api.errorBody  "No such Host"
// @Router       /host/{id}/local-forward [get]
func (h *Handler) ListHostLocalForwards(c echo.Context) error {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		return failure(c, http.StatusBadRequest, errHostIDInvalid, errorArgs{"reason": err.Error()})
	}

	page, refused := readListPage(c)
	if refused != nil {
		return badListPage(c, refused)
	}

	// The Host is read first for the reason ListHostServicePorts reads it: a
	// Host with no forwards and a Host that is not there are not one answer.
	var host models.Host
	err = h.db.First(&host, id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return failure(c, http.StatusNotFound, errHostNotFound)
		}
		h.logger.Error("failed to fetch Host", logid.HostFetchFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errHostFetchFailed)
	}

	var total int64
	err = h.db.Model(&models.LocalForward{}).Where("host_id = ?", host.ID).Count(&total).Error
	if err != nil {
		h.logger.Error("failed to count the local forwards", logid.LocalForwardCountFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errLocalForwardListFailed)
	}

	page = page.fitTo(total)

	var rows []models.LocalForward
	err = h.db.Where("host_id = ?", host.ID).Order("number").Limit(page.size).Offset(page.offset()).Find(&rows).Error
	if err != nil {
		h.logger.Error("failed to fetch local forwards", logid.LocalForwardListFetchFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errLocalForwardListFailed)
	}

	states := h.manager.LocalForwardStatuses()

	views := make([]localForwardView, 0, len(rows))
	for _, row := range rows {
		views = append(views, localForwardViewOf(row, host.Enabled, states))
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data: listPageOf{
			Items: views,
			Total: total,
			Page:  page.number,
			Size:  page.size,
		},
	})
}

// @Summary      Add a local forward to a Host
// @Description  This machine opens local_port and carries every connection to it over the SSH connection of the Host to target_address:target_port, as seen from the Host. target_address is a host name or an IP address, resolved by the Host.
// @Description  bind_scope is where local_port is opened on this machine: loopback, wildcard, or left out for the wildcard.
// @Description  enabled is whether the forward runs. Left out, it is true. A forward that is off opens no port and makes no SSH connection, and still holds local_port.
// @Description  allowed_sources is the addresses and CIDR blocks a client may connect to local_port from, separated by commas or spaces; empty or left out lets every address in. It is taken and held to on either bind_scope, the way socks_allowed_sources of a Host is.
// @Tags         local forwards
// @Accept   json
// @Produce  json
// @Security  CSRFToken
// @Param   id    path  int  true  "The id of the Host"
// @Param   body  body  models.LocalForwardRequest  true  "The local forward to add"
// @Success  201  {object}  models.Response{data=api.localForwardView}
// @Failure  400  {object}  api.errorBody  "The body is refused, carries the old name target_ip in place of target_address, or allowed_sources is not a list of addresses and CIDR blocks"
// @Failure  404  {object}  api.errorBody  "No such Host"
// @Failure  409  {object}  api.errorBody  "local_port is taken by another local forward or a SOCKS5 proxy, or is the port of this server"
// @Router       /host/{id}/local-forward [post]
func (h *Handler) CreateHostLocalForward(c echo.Context) error {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		return failure(c, http.StatusBadRequest, errHostIDInvalid, errorArgs{"reason": err.Error()})
	}

	refusedField := renamedFieldRefused(c, "target_ip", "target_address")
	if refusedField != nil {
		return refusedField.answer(c)
	}

	var req models.LocalForwardRequest
	err = c.Bind(&req)
	if err != nil {
		return failure(c, http.StatusBadRequest, errRequestBodyInvalid, errorArgs{"reason": err.Error()})
	}

	err = c.Validate(&req)
	if err != nil {
		return failure(c, http.StatusBadRequest, errRequestValidationFailed, errorArgs{"reason": err.Error()})
	}

	refusedSources := checkLocalForwardSources(req.AllowedSources)
	if refusedSources != nil {
		return refusedSources.answer(c)
	}

	apiPort, err := h.storedAPIPort()
	if err != nil {
		h.logger.Error("failed to read the settings", logid.SettingsReadFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errSettingsReadFailed)
	}

	tx := h.db.Begin()
	err = tx.Error
	if err != nil {
		h.logger.Error("failed to start the transaction", logid.DatabaseTransactionStartFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionBeginFailed)
	}
	defer rollbackUnlessDone(tx)

	var host models.Host
	err = tx.First(&host, id).Error
	if err != nil {
		tx.Rollback()
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return failure(c, http.StatusNotFound, errHostNotFound)
		}
		h.logger.Error("failed to fetch Host", logid.HostFetchFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errHostFetchFailed)
	}

	refused, err := localPortRefused(tx, 0, 0, req.LocalPort, apiPort, h.runningAPIPort)
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to fetch local forwards", logid.LocalForwardListFetchFailed.Field(),
			zap.Error(err), zap.Int("local_port", req.LocalPort))
		return failure(c, http.StatusInternalServerError, errLocalForwardCreateFailed)
	}
	if refused != nil {
		tx.Rollback()
		return refused.answer(c)
	}

	// An empty scope is stored as the word it stands for, so that the row says
	// what was chosen and every answer carries the same value for it.
	bindScope := req.BindScope
	if bindScope == "" {
		bindScope = models.BindScopeWildcard
	}

	number, err := nextLocalForwardNumber(tx, host.ID)
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to fetch local forwards", logid.LocalForwardListFetchFailed.Field(),
			zap.Error(err), zap.Uint("host_id", host.ID))
		return failure(c, http.StatusInternalServerError, errLocalForwardCreateFailed)
	}

	lf := models.LocalForward{
		HostID:        host.ID,
		Number:        number,
		BindScope:     bindScope,
		LocalPort:     req.LocalPort,
		TargetAddress: req.TargetAddress,
		TargetPort:    req.TargetPort,
		Description:   req.Description,
		Enabled:       req.Enabled == nil || *req.Enabled,
	}
	if req.AllowedSources != nil {
		lf.AllowedSources = *req.AllowedSources
	}

	err = tx.Create(&lf).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to create local forward", logid.LocalForwardCreateFailed.Field(),
			zap.Error(err), zap.Uint("host_id", host.ID))
		return failure(c, http.StatusInternalServerError, errLocalForwardCreateFailed)
	}

	err = tx.Commit().Error
	if err != nil {
		h.logger.Error("failed to commit the transaction", logid.DatabaseTransactionCommitFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionCommitFailed)
	}

	h.manager.WakeReconcile()

	return c.JSON(http.StatusCreated, models.Response{
		Success: true,
		Data:    localForwardViewOf(lf, host.Enabled, h.manager.LocalForwardStatuses()),
	})
}

// @Summary      Read one local forward of a Host
// @Description  A forward is named by the Host that carries it and its number on that Host. Numbers are handed out per Host, so the number alone names a forward on every Host and never one by itself.
// @Tags         local forwards
// @Produce  json
// @Param   id      path  int  true  "The id of the Host"
// @Param   number  path  int  true  "The number of the local forward on that Host"
// @Success  200  {object}  models.Response{data=api.localForwardView}
// @Failure  404  {object}  api.errorBody  "The Host carries no forward with that number"
// @Router       /host/{id}/local-forward/{number} [get]
func (h *Handler) GetLocalForward(c echo.Context) error {
	hostID, number, refused := localForwardPath(c)
	if refused != nil {
		return refused.answer(c)
	}

	var lf models.LocalForward
	err := h.db.Where("host_id = ? AND number = ?", hostID, number).First(&lf).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return failure(c, http.StatusNotFound, errLocalForwardNotFound)
		}
		h.logger.Error("failed to fetch local forward", logid.LocalForwardFetchFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errLocalForwardFetchFailed)
	}

	var host models.Host
	err = h.db.First(&host, lf.HostID).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		h.logger.Error("failed to fetch Host", logid.HostFetchFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errHostFetchFailed)
	}

	// A row whose Host is gone runs nothing, which is what a disabled Host
	// reports; the Host being deleted takes its forwards with it, so this is a
	// row left over from before that rule and not something to refuse over.
	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    localForwardViewOf(lf, host.Enabled, h.manager.LocalForwardStatuses()),
	})
}

// @Summary      Update a local forward of a Host
// @Description  local_port, target_address and target_port are all required. The Host it is carried by is not changed, and neither is its number.
// @Description  enabled switches the forward on or off. Left out, it keeps what is stored.
// @Description  allowed_sources left out keeps what is stored; sent empty it lets every address in.
// @Tags         local forwards
// @Accept   json
// @Produce  json
// @Security  CSRFToken
// @Param   id      path  int  true  "The id of the Host"
// @Param   number  path  int  true  "The number of the local forward on that Host"
// @Param   body  body  models.LocalForwardRequest  true  "The local forward as it should stand"
// @Success  200  {object}  models.Response{data=api.localForwardView}
// @Failure  400  {object}  api.errorBody  "The body is refused, carries the old name target_ip in place of target_address, or allowed_sources is not a list of addresses and CIDR blocks"
// @Failure  404  {object}  api.errorBody  "The Host carries no forward with that number"
// @Failure  409  {object}  api.errorBody  "local_port is taken by another local forward or a SOCKS5 proxy, or is the port of this server"
// @Router       /host/{id}/local-forward/{number} [put]
func (h *Handler) UpdateLocalForward(c echo.Context) error {
	hostID, number, refusedPath := localForwardPath(c)
	if refusedPath != nil {
		return refusedPath.answer(c)
	}

	refusedField := renamedFieldRefused(c, "target_ip", "target_address")
	if refusedField != nil {
		return refusedField.answer(c)
	}

	var req models.LocalForwardRequest
	err := c.Bind(&req)
	if err != nil {
		return failure(c, http.StatusBadRequest, errRequestBodyInvalid, errorArgs{"reason": err.Error()})
	}

	err = c.Validate(&req)
	if err != nil {
		return failure(c, http.StatusBadRequest, errRequestValidationFailed, errorArgs{"reason": err.Error()})
	}

	refusedSources := checkLocalForwardSources(req.AllowedSources)
	if refusedSources != nil {
		return refusedSources.answer(c)
	}

	apiPort, err := h.storedAPIPort()
	if err != nil {
		h.logger.Error("failed to read the settings", logid.SettingsReadFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errSettingsReadFailed)
	}

	tx := h.db.Begin()
	err = tx.Error
	if err != nil {
		h.logger.Error("failed to start the transaction", logid.DatabaseTransactionStartFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionBeginFailed)
	}
	defer rollbackUnlessDone(tx)

	// Read inside the transaction for the reason UpdateHost is.
	var lf models.LocalForward
	err = tx.Where("host_id = ? AND number = ?", hostID, number).First(&lf).Error
	if err != nil {
		tx.Rollback()
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return failure(c, http.StatusNotFound, errLocalForwardNotFound)
		}
		h.logger.Error("failed to fetch local forward", logid.LocalForwardFetchFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errLocalForwardFetchFailed)
	}

	refused, err := localPortRefused(tx, lf.HostID, lf.Number, req.LocalPort, apiPort, h.runningAPIPort)
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to fetch local forwards", logid.LocalForwardListFetchFailed.Field(),
			zap.Error(err), zap.Int("local_port", req.LocalPort))
		return failure(c, http.StatusInternalServerError, errLocalForwardUpdateFailed)
	}
	if refused != nil {
		tx.Rollback()
		return refused.answer(c)
	}

	var host models.Host
	err = tx.First(&host, lf.HostID).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		tx.Rollback()
		h.logger.Error("failed to fetch Host", logid.HostFetchFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errHostFetchFailed)
	}

	lf.BindScope = req.BindScope
	if lf.BindScope == "" {
		lf.BindScope = models.BindScopeWildcard
	}
	lf.LocalPort = req.LocalPort
	lf.TargetAddress = req.TargetAddress
	lf.TargetPort = req.TargetPort
	lf.Description = req.Description
	if req.Enabled != nil {
		lf.Enabled = *req.Enabled
	}
	if req.AllowedSources != nil {
		lf.AllowedSources = *req.AllowedSources
	}

	err = tx.Save(&lf).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to update local forward", logid.LocalForwardUpdateFailed.Field(),
			zap.Error(err), zap.Uint("local_forward_number", lf.Number))
		return failure(c, http.StatusInternalServerError, errLocalForwardUpdateFailed)
	}

	err = tx.Commit().Error
	if err != nil {
		h.logger.Error("failed to commit the transaction", logid.DatabaseTransactionCommitFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionCommitFailed)
	}

	h.manager.WakeReconcile()

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    localForwardViewOf(lf, host.Enabled, h.manager.LocalForwardStatuses()),
	})
}

// @Summary      Delete a local forward of a Host
// @Description  The number the forward carried is not handed out again, so a number in a log line is never read back against a row made after it.
// @Tags         local forwards
// @Produce  json
// @Security  CSRFToken
// @Param   id      path  int  true  "The id of the Host"
// @Param   number  path  int  true  "The number of the local forward on that Host"
// @Success  200  {object}  models.Response{data=string}
// @Failure  404  {object}  api.errorBody  "The Host carries no forward with that number"
// @Router       /host/{id}/local-forward/{number} [delete]
func (h *Handler) DeleteLocalForward(c echo.Context) error {
	hostID, number, refused := localForwardPath(c)
	if refused != nil {
		return refused.answer(c)
	}

	tx := h.db.Begin()
	err := tx.Error
	if err != nil {
		h.logger.Error("failed to start the transaction", logid.DatabaseTransactionStartFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionBeginFailed)
	}
	defer rollbackUnlessDone(tx)

	var lf models.LocalForward
	err = tx.Where("host_id = ? AND number = ?", hostID, number).First(&lf).Error
	if err != nil {
		tx.Rollback()
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return failure(c, http.StatusNotFound, errLocalForwardNotFound)
		}
		h.logger.Error("failed to fetch local forward", logid.LocalForwardFetchFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errLocalForwardFetchFailed)
	}

	err = tx.Delete(&lf).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to delete local forward", logid.LocalForwardDeleteFailed.Field(),
			zap.Error(err), zap.Uint("local_forward_number", lf.Number))
		return failure(c, http.StatusInternalServerError, errLocalForwardDeleteFailed)
	}

	err = tx.Commit().Error
	if err != nil {
		h.logger.Error("failed to commit the transaction", logid.DatabaseTransactionCommitFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionCommitFailed)
	}

	h.manager.WakeReconcile()

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    "Local forward deleted successfully",
	})
}
