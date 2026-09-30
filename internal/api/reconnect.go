package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// ReconnectHost drops every connection of one Host and makes it again.
//
// It is what a change made on the Host itself needs, a change nothing here can
// see. An SSH server reads its configuration once for each connection, so a
// forward opened before GatewayPorts was changed stays on the addresses it was
// opened on, and asking for it again on the same connection is answered under
// the old setting. A new connection is served under the new one.
//
// Nothing about the Host is written. Its tunnels, local forwards and SOCKS5
// proxy are stopped by the next reconcile pass and started again by the same
// pass, so a Host that is disabled, or that carries nothing, has nothing
// dropped and is answered with zero counts rather than refused.
//
// @Summary      Drop every connection of this Host and make it again
// @Description  Its service port tunnels, local forwards and SOCKS5 proxy are stopped and started again on new SSH connections by the reconcile pass this request wakes. That is what a change to the SSH server of the Host, GatewayPorts among them, needs: the server reads its configuration once for each connection.
// @Description  The counts are what was running over the Host when the request came, which is what is made again. A Host that is disabled or carries nothing is answered with zero counts.
// @Tags         hosts
// @Produce  json
// @Security  CSRFToken
// @Param   id  path  int  true  "The id of the Host"
// @Success  200  {object}  models.Response{data=tunnel.ReconnectCounts}
// @Failure  400  {object}  api.errorBody  "The id is not a number"
// @Failure  404  {object}  api.errorBody  "No such Host"
// @Router       /host/{id}/reconnect [post]
func (h *Handler) ReconnectHost(c echo.Context) error {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		return failure(c, http.StatusBadRequest, errHostIDInvalid, errorArgs{"reason": err.Error()})
	}

	var host models.Host
	err = h.db.First(&host, id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return failure(c, http.StatusNotFound, errHostNotFound)
		}
		h.logger.Error("failed to fetch Host", logid.HostFetchFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errHostFetchFailed)
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    h.manager.RequestReconnect(host.ID),
	})
}
