package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// tunnelManager is what the handlers need from the tunnel manager. They write
// rows and ask for a reconcile pass; starting and stopping tunnels is the work
// of the loop, so nothing here does it.
type tunnelManager interface {
	// WakeReconcile asks for a reconcile pass. It is called once the
	// transaction is committed, because a pass reads the rows and a pass that
	// runs before the commit does not see them.
	WakeReconcile()
	GetAllTunnels() (*[]models.Tunnel, error)
	GetHostTunnels(hostID uint) (*[]models.Tunnel, error)
}

type Handler struct {
	db      *gorm.DB
	manager tunnelManager
	logger  *zap.Logger
	cipher  *crypto.Cipher
}

func NewHandler(db *gorm.DB, manager tunnelManager, logger *zap.Logger, cipher *crypto.Cipher) *Handler {
	return &Handler{
		db:      db,
		manager: manager,
		logger:  logger,
		cipher:  cipher,
	}
}

func (h *Handler) CreateHost(c echo.Context) error {
	var req models.CreateHostRequest
	err := c.Bind(&req)
	if err != nil {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "Invalid request body: " + err.Error(),
		})
	}

	err = c.Validate(&req)
	if err != nil {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "Validation failed: " + err.Error(),
		})
	}

	password, err := h.cipher.Encrypt(req.Password)
	if err != nil {
		h.logger.Error("failed to encrypt the password of the Host", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to encrypt the password",
		})
	}

	tx := h.db.Begin()
	err = tx.Error
	if err != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to start transaction: " + err.Error(),
		})
	}

	host := &models.Host{
		IP:          req.IP,
		Port:        req.Port,
		User:        req.User,
		Password:    password,
		Description: req.Description,
	}

	err = tx.Create(host).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to create Host", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to create Host: " + err.Error(),
		})
	}

	err = tx.Commit().Error
	if err != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to commit transaction: " + err.Error(),
		})
	}

	h.manager.WakeReconcile()

	return c.JSON(http.StatusCreated, models.Response{
		Success: true,
		Data:    host,
	})
}

func (h *Handler) ListHosts(c echo.Context) error {
	var hosts []models.Host
	err := h.db.Find(&hosts).Error
	if err != nil {
		h.logger.Error("failed to fetch Hosts", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch Hosts: " + err.Error(),
		})
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    hosts,
	})
}

func (h *Handler) GetHost(c echo.Context) error {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "Invalid Host ID: " + err.Error(),
		})
	}

	var host models.Host
	err = h.db.First(&host, id).Error
	if err != nil {
		return c.JSON(http.StatusNotFound, models.Response{
			Success: false,
			Error:   "Host not found: " + err.Error(),
		})
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    host,
	})
}

func (h *Handler) UpdateHost(c echo.Context) error {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "Invalid Host ID: " + err.Error(),
		})
	}

	var req models.UpdateHostRequest
	err = c.Bind(&req)
	if err != nil {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "Invalid request body: " + err.Error(),
		})
	}

	err = c.Validate(&req)
	if err != nil {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "Validation failed: " + err.Error(),
		})
	}

	var host models.Host
	err = h.db.First(&host, id).Error
	if err != nil {
		return c.JSON(http.StatusNotFound, models.Response{
			Success: false,
			Error:   "Host not found: " + err.Error(),
		})
	}

	if req.IP != "" {
		host.IP = req.IP
	}
	if req.Port != nil {
		host.Port = *req.Port
	}
	if req.User != "" {
		host.User = req.User
	}
	if req.Password != "" {
		password, err := h.cipher.Encrypt(req.Password)
		if err != nil {
			h.logger.Error("failed to encrypt the password of the Host", zap.Error(err))
			return c.JSON(http.StatusInternalServerError, models.Response{
				Success: false,
				Error:   "Failed to encrypt the password",
			})
		}
		host.Password = password
	}
	if req.Description != "" {
		host.Description = req.Description
	}
	if req.Enabled != nil {
		host.Enabled = *req.Enabled
	}

	tx := h.db.Begin()
	err = tx.Error
	if err != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to start transaction: " + err.Error(),
		})
	}

	err = tx.Save(&host).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to update Host", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to update Host: " + err.Error(),
		})
	}

	err = tx.Commit().Error
	if err != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to commit transaction: " + err.Error(),
		})
	}

	h.manager.WakeReconcile()

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    host,
	})
}

func (h *Handler) DeleteHost(c echo.Context) error {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "Invalid Host ID: " + err.Error(),
		})
	}

	var host models.Host
	err = h.db.First(&host, id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return c.JSON(http.StatusNotFound, models.Response{
				Success: false,
				Error:   "Host not found",
			})
		}
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch Host: " + err.Error(),
		})
	}

	tx := h.db.Begin()
	err = tx.Error
	if err != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to start transaction: " + err.Error(),
		})
	}

	err = tx.Delete(&host).Error
	if err != nil {
		tx.Rollback()
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to delete Host: " + err.Error(),
		})
	}

	err = tx.Commit().Error
	if err != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to commit transaction: " + err.Error(),
		})
	}

	h.manager.WakeReconcile()

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    "Host deleted successfully",
	})
}

func (h *Handler) CreateServicePort(c echo.Context) error {
	var req models.CreateServicePortRequest
	err := c.Bind(&req)
	if err != nil {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "Invalid request body: " + err.Error(),
		})
	}

	err = c.Validate(&req)
	if err != nil {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "Validation failed: " + err.Error(),
		})
	}

	sp := &models.ServicePort{
		ServiceIP:   req.ServiceIP,
		ServicePort: req.ServicePort,
		LocalPort:   req.LocalPort,
		Description: req.Description,
	}

	tx := h.db.Begin()
	err = tx.Error
	if err != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to start transaction: " + err.Error(),
		})
	}

	err = tx.Create(sp).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to create service port", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to create service port: " + err.Error(),
		})
	}

	err = tx.Commit().Error
	if err != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to commit transaction: " + err.Error(),
		})
	}

	// The row is stored, which is what the answer reports. Whether a tunnel can
	// be built for it is answered by the status of the tunnels, not here.
	h.manager.WakeReconcile()

	return c.JSON(http.StatusCreated, models.Response{
		Success: true,
		Data:    sp,
	})
}

func (h *Handler) ListServicePorts(c echo.Context) error {
	var sps []models.ServicePort
	err := h.db.Find(&sps).Error
	if err != nil {
		h.logger.Error("failed to fetch service ports", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch service ports: " + err.Error(),
		})
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    sps,
	})
}

func (h *Handler) GetServicePort(c echo.Context) error {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "Invalid service port ID: " + err.Error(),
		})
	}

	var sp models.ServicePort
	err = h.db.First(&sp, id).Error
	if err != nil {
		return c.JSON(http.StatusNotFound, models.Response{
			Success: false,
			Error:   "Service port not found: " + err.Error(),
		})
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    sp,
	})
}

func (h *Handler) UpdateServicePort(c echo.Context) error {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "Invalid service port ID: " + err.Error(),
		})
	}

	var sp models.ServicePort
	err = h.db.First(&sp, id).Error
	if err != nil {
		return c.JSON(http.StatusNotFound, models.Response{
			Success: false,
			Error:   "Service port not found: " + err.Error(),
		})
	}
	var req models.CreateServicePortRequest
	err = c.Bind(&req)
	if err != nil {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "Invalid request body: " + err.Error(),
		})
	}

	err = c.Validate(&req)
	if err != nil {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "Validation failed: " + err.Error(),
		})
	}

	tx := h.db.Begin()
	err = tx.Error
	if err != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to start transaction: " + err.Error(),
		})
	}

	sp.ServiceIP = req.ServiceIP
	sp.ServicePort = req.ServicePort
	sp.LocalPort = req.LocalPort
	sp.Description = req.Description

	err = tx.Save(&sp).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to update service port", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to update service port: " + err.Error(),
		})
	}

	err = tx.Commit().Error
	if err != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to commit transaction: " + err.Error(),
		})
	}

	h.manager.WakeReconcile()

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    sp,
	})
}

func (h *Handler) DeleteServicePort(c echo.Context) error {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "Invalid service port ID: " + err.Error(),
		})
	}

	var sp models.ServicePort
	err = h.db.First(&sp, id).Error
	if err != nil {
		return c.JSON(http.StatusNotFound, models.Response{
			Success: false,
			Error:   "Service port not found: " + err.Error(),
		})
	}

	tx := h.db.Begin()
	err = tx.Error
	if err != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to start transaction: " + err.Error(),
		})
	}

	err = tx.Delete(&sp).Error
	if err != nil {
		tx.Rollback()
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to delete service port: " + err.Error(),
		})
	}

	err = tx.Commit().Error
	if err != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to commit transaction: " + err.Error(),
		})
	}

	h.manager.WakeReconcile()

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    "Service port deleted successfully",
	})
}

func (h *Handler) GetStatus(c echo.Context) error {
	tunnels, err := h.manager.GetAllTunnels()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch tunnel status: " + err.Error(),
		})
	}

	var connectedTunnels int
	for _, t := range *tunnels {
		if t.Status == "connected" {
			connectedTunnels++
		}
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data: map[string]interface{}{
			"total_tunnels":     len(*tunnels),
			"connected_tunnels": connectedTunnels,
			"tunnels":           tunnels,
		},
	})
}

func (h *Handler) GetHostStatus(c echo.Context) error {
	hostID, err := strconv.ParseUint(c.Param("hostId"), 10, 32)
	if err != nil {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "Invalid Host ID: " + err.Error(),
		})
	}

	var host models.Host
	err = h.db.First(&host, hostID).Error
	if err != nil {
		return c.JSON(http.StatusNotFound, models.Response{
			Success: false,
			Error:   "Host not found: " + err.Error(),
		})
	}

	tunnels, err := h.manager.GetHostTunnels(uint(hostID))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch tunnel status: " + err.Error(),
		})
	}

	var connectedTunnels int
	for _, t := range *tunnels {
		if t.Status == "connected" {
			connectedTunnels++
		}
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data: map[string]interface{}{
			"host":              host,
			"total_tunnels":     len(*tunnels),
			"connected_tunnels": connectedTunnels,
			"tunnels":           tunnels,
		},
	})
}
