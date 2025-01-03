package api

import (
	"errors"
	"fmt"
	"gorm.io/gorm"
	"net/http"
	"strconv"

	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/tunnel"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

type Handler struct {
	db      *gorm.DB
	manager *tunnel.Manager
	logger  *zap.Logger
}

func NewHandler(db *gorm.DB, manager *tunnel.Manager, logger *zap.Logger) *Handler {
	return &Handler{
		db:      db,
		manager: manager,
		logger:  logger,
	}
}

func (h *Handler) CreateVM(c echo.Context) error {
	var req models.CreateVMRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "Invalid request body",
		})
	}

	// Start transaction
	tx := h.db.Begin()
	if tx.Error != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to start transaction",
		})
	}

	vm := &models.VM{
		IP:          req.IP,
		Port:        req.Port,
		User:        req.User,
		Password:    req.Password,
		Description: req.Description,
	}

	// Create VM
	if err := tx.Create(vm).Error; err != nil {
		tx.Rollback()
		h.logger.Error("failed to create VM", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to create VM",
		})
	}

	var sps []models.ServicePort
	if err := h.db.Find(&sps).Error; err != nil {
		h.logger.Error("failed to fetch service ports", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch service ports",
		})
	}

	// Start new tunnels
	for _, sp := range sps {
		if err := h.manager.StartTunnel(vm, &sp); err != nil {
			h.logger.Error("failed to restart tunnel",
				zap.Error(err),
				zap.String("vm_ip", vm.IP),
				zap.Int("service_port", sp.ServicePort))
			// Continue attempting to start other tunnels
		}
	}

	if err := tx.Commit().Error; err != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to commit transaction",
		})
	}

	return c.JSON(http.StatusCreated, models.Response{
		Success: true,
		Data:    vm,
	})
}

func (h *Handler) ListVMs(c echo.Context) error {
	var vms []models.VM
	if err := h.db.Preload("Tunnels").Find(&vms).Error; err != nil {
		h.logger.Error("failed to fetch VMs", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch VMs",
		})
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    vms,
	})
}

func (h *Handler) GetVM(c echo.Context) error {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "Invalid VM ID",
		})
	}

	var vm models.VM
	if err := h.db.Preload("Tunnels").First(&vm, id).Error; err != nil {
		return c.JSON(http.StatusNotFound, models.Response{
			Success: false,
			Error:   "VM not found",
		})
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    vm,
	})
}

func (h *Handler) UpdateVM(c echo.Context) error {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "Invalid VM ID",
		})
	}

	var vm models.VM
	if err := h.db.First(&vm, id).Error; err != nil {
		return c.JSON(http.StatusNotFound, models.Response{
			Success: false,
			Error:   "VM not found",
		})
	}

	var sps []models.ServicePort
	if err := h.db.Find(&sps).Error; err != nil {
		h.logger.Error("failed to fetch service ports", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch service ports",
		})
	}

	var req models.CreateVMRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "Invalid request body",
		})
	}

	// Start transaction
	tx := h.db.Begin()
	if tx.Error != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to start transaction",
		})
	}

	// Check if critical fields are being updated
	needTunnelRestart := vm.IP != req.IP || vm.Port != req.Port || vm.User != req.User || vm.Password != req.Password

	// Update VM
	vm.IP = req.IP
	vm.Port = req.Port
	vm.User = req.User
	vm.Password = req.Password
	vm.Description = req.Description

	if err := tx.Save(&vm).Error; err != nil {
		tx.Rollback()
		h.logger.Error("failed to update VM", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to update VM",
		})
	}

	// Restart tunnels if necessary
	if needTunnelRestart && len(sps) > 0 {
		// Stop all existing tunnels
		for _, sp := range sps {
			if err := h.manager.StopTunnel(vm.ID, sp.ID); err != nil {
				h.logger.Warn("failed to stop tunnel",
					zap.Uint("vm_id", vm.ID),
					zap.Uint("service_port_id", sp.ID),
					zap.Error(err))
			}
		}

		// Start new tunnels
		for _, sp := range sps {
			if err := h.manager.StartTunnel(&vm, &sp); err != nil {
				h.logger.Error("failed to restart tunnel",
					zap.Error(err),
					zap.String("vm_ip", vm.IP),
					zap.Int("service_port", sp.ServicePort))
				// Continue attempting to start other tunnels
			}
		}

		// Update tunnel status
		if err := tx.Model(&models.Tunnel{}).
			Where("vm_id = ?", vm.ID).
			Update("status", "restarted").Error; err != nil {
			tx.Rollback()
			return c.JSON(http.StatusInternalServerError, models.Response{
				Success: false,
				Error:   "Failed to update tunnel status",
			})
		}
	}

	if err := tx.Commit().Error; err != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to commit transaction",
		})
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    vm,
	})
}

func (h *Handler) DeleteVM(c echo.Context) error {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "Invalid VM ID",
		})
	}

	// Start transaction
	tx := h.db.Begin()
	if tx.Error != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to start transaction",
		})
	}

	// Lock the VM record for update
	var vm models.VM
	if err := tx.Set("gorm:pessimistic_lock", true).First(&vm, id).Error; err != nil {
		tx.Rollback()
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return c.JSON(http.StatusNotFound, models.Response{
				Success: false,
				Error:   "VM not found",
			})
		}
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to lock VM record",
		})
	}

	// Get all service ports in a single query
	var servicePorts []models.ServicePort
	if err := tx.Where("vm_id = ?", id).Find(&servicePorts).Error; err != nil {
		tx.Rollback()
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch service ports",
		})
	}

	// Stop all tunnels
	for _, sp := range servicePorts {
		if err := h.manager.StopTunnel(uint(id), sp.ID); err != nil {
			h.logger.Warn("failed to stop tunnel",
				zap.Uint("vm_id", uint(id)),
				zap.Uint("service_port_id", sp.ID),
				zap.Error(err))
		}
	}

	// Delete tunnels first
	if err := tx.Unscoped().Where("vm_id = ?", id).Delete(&models.Tunnel{}).Error; err != nil {
		tx.Rollback()
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to delete tunnels",
		})
	}

	// Delete service ports
	if err := tx.Unscoped().Where("vm_id = ?", id).Delete(&models.ServicePort{}).Error; err != nil {
		tx.Rollback()
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to delete service ports",
		})
	}

	// Finally delete the VM
	if err := tx.Unscoped().Delete(&vm).Error; err != nil {
		tx.Rollback()
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to delete VM",
		})
	}

	// Commit transaction
	if err := tx.Commit().Error; err != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to commit transaction",
		})
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    "VM deleted successfully",
	})
}

func (h *Handler) CreateServicePort(c echo.Context) error {
	var req models.CreateServicePortRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "Invalid request body",
		})
	}

	sp := &models.ServicePort{
		ServiceIP:   req.ServiceIP,
		ServicePort: req.ServicePort,
		LocalPort:   req.LocalPort,
		Description: req.Description,
	}

	// Start DB transaction
	tx := h.db.Begin()
	if tx.Error != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to start transaction",
		})
	}

	// Create service port
	if err := tx.Create(sp).Error; err != nil {
		tx.Rollback()
		h.logger.Error("failed to create service port", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to create service port",
		})
	}

	var vms []models.VM
	if err := h.db.Preload("Tunnels").Find(&vms).Error; err != nil {
		h.logger.Error("failed to fetch VMs", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch VMs",
		})
	}

	// Start new tunnels
	for _, vm := range vms {
		if err := h.manager.StartTunnel(&vm, sp); err != nil {
			tx.Rollback()
			h.logger.Error("failed to start new tunnel",
				zap.Error(err),
				zap.String("vm_ip", vm.IP),
				zap.Int("service_port", sp.ServicePort))
			return c.JSON(http.StatusInternalServerError, models.Response{
				Success: false,
				Error:   fmt.Sprintf("Failed to start new tunnel: %v", err),
			})
		}
	}

	// Commit transaction
	if err := tx.Commit().Error; err != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to commit transaction",
		})
	}

	return c.JSON(http.StatusCreated, models.Response{
		Success: true,
		Data:    sp,
	})
}

func (h *Handler) ListServicePorts(c echo.Context) error {
	var sps []models.ServicePort
	if err := h.db.Find(&sps).Error; err != nil {
		h.logger.Error("failed to fetch service ports", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch service ports",
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
			Error:   "Invalid service port ID",
		})
	}

	var sp models.ServicePort
	if err := h.db.First(&sp, id).Error; err != nil {
		return c.JSON(http.StatusNotFound, models.Response{
			Success: false,
			Error:   "Service port not found",
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
			Error:   "Invalid service port ID",
		})
	}

	// Find existing service port
	var sp models.ServicePort
	if err := h.db.First(&sp, id).Error; err != nil {
		return c.JSON(http.StatusNotFound, models.Response{
			Success: false,
			Error:   "Service port not found",
		})
	}

	// Get request body
	var req models.CreateServicePortRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "Invalid request body",
		})
	}

	// Start transaction
	tx := h.db.Begin()
	if tx.Error != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to start transaction",
		})
	}

	var vms []models.VM
	if err := h.db.Preload("Tunnels").Find(&vms).Error; err != nil {
		h.logger.Error("failed to fetch VMs", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch VMs",
		})
	}

	// Stop existing tunnels
	for _, vm := range vms {
		if err := h.manager.StopTunnel(vm.ID, sp.ID); err != nil {
			h.logger.Warn("failed to stop existing tunnel",
				zap.String("vm_ip", vm.IP),
				zap.Int("service_port", sp.ServicePort),
				zap.Error(err))
		}
	}

	// Update service port
	sp.ServiceIP = req.ServiceIP
	sp.ServicePort = req.ServicePort
	sp.LocalPort = req.LocalPort
	sp.Description = req.Description

	if err := tx.Save(&sp).Error; err != nil {
		tx.Rollback()
		h.logger.Error("failed to update service port", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to update service port",
		})
	}

	// Start new tunnels
	for _, vm := range vms {
		if err := h.manager.StartTunnel(&vm, &sp); err != nil {
			tx.Rollback()
			h.logger.Error("failed to start new tunnel",
				zap.Error(err),
				zap.String("vm_ip", vm.IP),
				zap.Int("service_port", sp.ServicePort))
			return c.JSON(http.StatusInternalServerError, models.Response{
				Success: false,
				Error:   fmt.Sprintf("Failed to start new tunnel: %v", err),
			})
		}
	}

	// Commit transaction
	if err := tx.Commit().Error; err != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to commit transaction",
		})
	}

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
			Error:   "Invalid service port ID",
		})
	}

	// Find service port
	var sp models.ServicePort
	if err := h.db.First(&sp, id).Error; err != nil {
		return c.JSON(http.StatusNotFound, models.Response{
			Success: false,
			Error:   "Service port not found",
		})
	}

	// Start transaction
	tx := h.db.Begin()
	if tx.Error != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to start transaction",
		})
	}

	var vms []models.VM
	if err := h.db.Preload("Tunnels").Find(&vms).Error; err != nil {
		h.logger.Error("failed to fetch VMs", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch VMs",
		})
	}

	// Stop tunnel first
	for _, vm := range vms {
		if err := h.manager.StopTunnel(vm.ID, sp.ID); err != nil {
			h.logger.Warn("failed to stop tunnel",
				zap.String("vm_ip", vm.IP),
				zap.Int("service_port", sp.ServicePort),
				zap.Error(err))
		}
	}

	// Delete service port
	if err := tx.Delete(&sp).Error; err != nil {
		tx.Rollback()
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to delete service port",
		})
	}

	// Commit transaction
	if err := tx.Commit().Error; err != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to commit transaction",
		})
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    "Service port deleted successfully",
	})
}

func (h *Handler) GetStatus(c echo.Context) error {
	var tunnels []models.Tunnel
	if err := h.db.Preload("VM").Find(&tunnels).Error; err != nil {
		h.logger.Error("failed to fetch tunnel status", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch tunnel status",
		})
	}

	// Get active tunnel count from manager
	activeTunnels := len(h.manager.GetActiveTunnels())

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data: map[string]interface{}{
			"tunnels":      tunnels,
			"active_count": activeTunnels,
			"total_count":  len(tunnels),
		},
	})
}

func (h *Handler) GetVMStatus(c echo.Context) error {
	vmID, err := strconv.ParseUint(c.Param("vmId"), 10, 32)
	if err != nil {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "Invalid VM ID",
		})
	}

	var vm models.VM
	if err := h.db.Preload("Tunnels").First(&vm, vmID).Error; err != nil {
		return c.JSON(http.StatusNotFound, models.Response{
			Success: false,
			Error:   "VM not found",
		})
	}

	// Get active tunnels for this VM
	activeTunnels := h.manager.GetVMActiveTunnels(uint(vmID))

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data: map[string]interface{}{
			"vm":             vm,
			"tunnels":        vm.Tunnels,
			"active_tunnels": len(activeTunnels),
			"total_tunnels":  len(vm.Tunnels),
		},
	})
}
