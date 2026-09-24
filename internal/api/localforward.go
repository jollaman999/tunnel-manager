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

// The two statuses a row carries when no forward runs for it. The manager
// reports nothing for such a row, and the two reasons are kept apart because
// they leave the operator with different work: a disabled Host is switched
// on, and a forward that is not running under an enabled Host is either
// waiting for the next reconcile pass or failed to start, which the log says.
const (
	localForwardStatusDisabled = "disabled"
	localForwardStatusStopped  = "stopped"
)

// localForwardView is one local forward as an answer carries it: the row, with
// what the running forward reports laid over it. The state is spelled out
// rather than embedded as tunnel.LocalForwardState so that the description
// generated from these types does not have to reach into package tunnel.
type localForwardView struct {
	models.LocalForward
	// Status is what tunnel.LocalForwardState reports while a forward runs,
	// and "disabled" or "stopped" while none does.
	Status          string    `json:"status"`
	LastError       string    `json:"last_error"`
	RetryCount      int       `json:"retry_count"`
	LastConnectedAt time.Time `json:"last_connected_at"`
}

// localForwardViewOf builds the view of one row. The scope goes out as a word
// every time, since an empty one is the wildcard and a screen that had to know
// that would be a second place keeping the rule.
func localForwardViewOf(lf models.LocalForward, hostEnabled bool, states map[uint]tunnel.LocalForwardState) localForwardView {
	if lf.BindScope == "" {
		lf.BindScope = models.BindScopeWildcard
	}

	view := localForwardView{LocalForward: lf}

	state, running := states[lf.ID]
	switch {
	case !hostEnabled:
		view.Status = localForwardStatusDisabled
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
// The stored port is the one checked rather than the one this process came up
// on, because a forward is kept for longer than this process runs and the
// stored port is the one every later startup listens on.
func (h *Handler) storedAPIPort() (int, error) {
	stored, err := settings.Load(h.db)
	if err != nil {
		return 0, err
	}

	return stored.APIPort, nil
}

// localPortRefused checks the local port of a forward against the two things
// on this machine it must not meet: the port of this server, and the port of
// another forward. id is the row being changed, or zero for a new one. A
// non-nil refusal is the answer; a non-nil error is a failed read, which the
// caller answers with the failure of its own write.
func localPortRefused(tx *gorm.DB, id uint, localPort int, apiPort int) (*refusal, error) {
	if localPort == apiPort {
		return refuse(http.StatusConflict, errLocalForwardPortIsAPIPort,
			errorArgs{"local_port": strconv.Itoa(localPort)}), nil
	}

	var taken int64

	err := tx.Model(&models.LocalForward{}).Where("local_port = ? AND id <> ?", localPort, id).Count(&taken).Error
	if err != nil {
		return nil, err
	}

	if taken > 0 {
		return refuse(http.StatusConflict, errLocalForwardPortTaken,
			errorArgs{"local_port": strconv.Itoa(localPort)}), nil
	}

	return nil, nil
}

// apiPortHolder is the local forward that opens the port a change of the
// settings asked to store as api_port, as the refusal of that change carries it.
type apiPortHolder struct {
	ID         uint   `json:"id"`
	HostID     uint   `json:"host_id"`
	HostIP     string `json:"host_ip"`
	LocalPort  int    `json:"local_port"`
	TargetIP   string `json:"target_ip"`
	TargetPort int    `json:"target_port"`
}

// apiPortTaken is the data of that refusal: the forward in the way, and a port
// either of the two could move to. SuggestedPort is 0 when no port is free.
type apiPortTaken struct {
	LocalForward  apiPortHolder `json:"local_forward"`
	SuggestedPort int           `json:"suggested_port"`
}

// apiPortRefused checks a port asked for as api_port against the local ports
// of the forwards. code is the refusal the caller answers under, and
// storedPort is the api_port stored now, which the suggested port stays clear
// of as well. Like localPortRefused, a non-nil error is a failed read.
func apiPortRefused(tx *gorm.DB, code errorCode, apiPort int, storedPort int) (*refusal, error) {
	var holder models.LocalForward

	err := tx.Where("local_port = ?", apiPort).First(&holder).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	// A row whose Host is gone is named by the number, as an import names it.
	hostIP := ""
	host := strconv.FormatUint(uint64(holder.HostID), 10)

	var owner models.Host
	if tx.First(&owner, holder.HostID).Error == nil {
		hostIP = owner.IP
		host = owner.IP
	}

	suggested, err := freeLocalPort(tx, apiPort, storedPort)
	if err != nil {
		return nil, err
	}

	return refuse(http.StatusConflict, code, errorArgs{
		"api_port": strconv.Itoa(apiPort),
		"host":     host,
		"target":   net.JoinHostPort(holder.TargetIP, strconv.Itoa(holder.TargetPort)),
	}).carrying(apiPortTaken{
		LocalForward: apiPortHolder{
			ID:         holder.ID,
			HostID:     holder.HostID,
			HostIP:     hostIP,
			LocalPort:  holder.LocalPort,
			TargetIP:   holder.TargetIP,
			TargetPort: holder.TargetPort,
		},
		SuggestedPort: suggested,
	}), nil
}

// freeLocalPort is the first port after taken that no local forward opens and
// that is neither taken nor storedPort. It goes up to 65535 and goes on from
// 1024, below which a port is one the operator picks on purpose; 0 is none.
func freeLocalPort(tx *gorm.DB, taken int, storedPort int) (int, error) {
	var ports []int

	err := tx.Model(&models.LocalForward{}).Pluck("local_port", &ports).Error
	if err != nil {
		return 0, err
	}

	held := map[int]bool{taken: true, storedPort: true}
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

// ListHostLocalForwards answers every local forward of one Host. It is not
// paged: the rows of one Host are few, and the panel that draws them shows
// them together.
//
// @Summary      The local forwards of a Host, with what each one reports
// @Description  status is what the running forward reports (starting, connected, reconnecting, error, host_key_unapproved, host_key_mismatch), "disabled" when the Host is disabled, and "stopped" when the Host is enabled and no forward runs yet or it failed to start.
// @Description  bind_scope is always loopback or wildcard.
// @Tags         local forwards
// @Produce  json
// @Param   id  path  int  true  "The id of the Host"
// @Success  200  {object}  models.Response{data=[]api.localForwardView}
// @Failure  404  {object}  api.errorBody  "No such Host"
// @Router       /host/{id}/local-forward [get]
func (h *Handler) ListHostLocalForwards(c echo.Context) error {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		return failure(c, http.StatusBadRequest, errHostIDInvalid, errorArgs{"reason": err.Error()})
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

	var rows []models.LocalForward
	err = h.db.Where("host_id = ?", host.ID).Order("id").Find(&rows).Error
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
		Data:    views,
	})
}

// @Summary      Add a local forward to a Host
// @Description  This machine opens local_port and carries every connection to it over the SSH connection of the Host to target_ip:target_port, as seen from the Host.
// @Description  bind_scope is where local_port is opened on this machine: loopback, wildcard, or left out for the wildcard.
// @Tags         local forwards
// @Accept   json
// @Produce  json
// @Security  CSRFToken
// @Param   id    path  int  true  "The id of the Host"
// @Param   body  body  models.LocalForwardRequest  true  "The local forward to add"
// @Success  201  {object}  models.Response{data=api.localForwardView}
// @Failure  400  {object}  api.errorBody  "The body is refused"
// @Failure  404  {object}  api.errorBody  "No such Host"
// @Failure  409  {object}  api.errorBody  "local_port is taken by another local forward or is the port of this server"
// @Router       /host/{id}/local-forward [post]
func (h *Handler) CreateHostLocalForward(c echo.Context) error {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		return failure(c, http.StatusBadRequest, errHostIDInvalid, errorArgs{"reason": err.Error()})
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

	refused, err := localPortRefused(tx, 0, req.LocalPort, apiPort)
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

	lf := models.LocalForward{
		HostID:      host.ID,
		BindScope:   bindScope,
		LocalPort:   req.LocalPort,
		TargetIP:    req.TargetIP,
		TargetPort:  req.TargetPort,
		Description: req.Description,
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

// @Summary      Read one local forward
// @Tags         local forwards
// @Produce  json
// @Param   id  path  int  true  "The id of the local forward"
// @Success  200  {object}  models.Response{data=api.localForwardView}
// @Failure  404  {object}  api.errorBody  "No such local forward"
// @Router       /local-forward/{id} [get]
func (h *Handler) GetLocalForward(c echo.Context) error {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		return failure(c, http.StatusBadRequest, errLocalForwardIDInvalid, errorArgs{"reason": err.Error()})
	}

	var lf models.LocalForward
	err = h.db.First(&lf, id).Error
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

// @Summary      Update a local forward
// @Description  local_port, target_ip and target_port are all required. The Host it is carried by is not changed.
// @Tags         local forwards
// @Accept   json
// @Produce  json
// @Security  CSRFToken
// @Param   id    path  int  true  "The id of the local forward"
// @Param   body  body  models.LocalForwardRequest  true  "The local forward as it should stand"
// @Success  200  {object}  models.Response{data=api.localForwardView}
// @Failure  400  {object}  api.errorBody  "The body is refused"
// @Failure  404  {object}  api.errorBody  "No such local forward"
// @Failure  409  {object}  api.errorBody  "local_port is taken by another local forward or is the port of this server"
// @Router       /local-forward/{id} [put]
func (h *Handler) UpdateLocalForward(c echo.Context) error {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		return failure(c, http.StatusBadRequest, errLocalForwardIDInvalid, errorArgs{"reason": err.Error()})
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

	// Read inside the transaction for the reason UpdateHost is.
	var lf models.LocalForward
	err = tx.First(&lf, id).Error
	if err != nil {
		tx.Rollback()
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return failure(c, http.StatusNotFound, errLocalForwardNotFound)
		}
		h.logger.Error("failed to fetch local forward", logid.LocalForwardFetchFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errLocalForwardFetchFailed)
	}

	refused, err := localPortRefused(tx, lf.ID, req.LocalPort, apiPort)
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
	lf.TargetIP = req.TargetIP
	lf.TargetPort = req.TargetPort
	lf.Description = req.Description

	err = tx.Save(&lf).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to update local forward", logid.LocalForwardUpdateFailed.Field(),
			zap.Error(err), zap.Uint("local_forward_id", lf.ID))
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

// @Summary      Delete a local forward
// @Tags         local forwards
// @Produce  json
// @Security  CSRFToken
// @Param   id  path  int  true  "The id of the local forward"
// @Success  200  {object}  models.Response{data=string}
// @Failure  404  {object}  api.errorBody  "No such local forward"
// @Router       /local-forward/{id} [delete]
func (h *Handler) DeleteLocalForward(c echo.Context) error {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		return failure(c, http.StatusBadRequest, errLocalForwardIDInvalid, errorArgs{"reason": err.Error()})
	}

	tx := h.db.Begin()
	err = tx.Error
	if err != nil {
		h.logger.Error("failed to start the transaction", logid.DatabaseTransactionStartFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionBeginFailed)
	}

	var lf models.LocalForward
	err = tx.First(&lf, id).Error
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
			zap.Error(err), zap.Uint("local_forward_id", lf.ID))
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
