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

	tx := h.db.Begin()
	err = tx.Error
	if tx.Error != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to start transaction: " + err.Error(),
		})
	}

	vm := &models.VM{
		IP:          req.IP,
		Port:        req.Port,
		User:        req.User,
		Password:    req.Password,
		Description: req.Description,
	}

	err = tx.Create(vm).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to create VM", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to create VM: " + err.Error(),
		})
	}

	var sps []models.ServicePort
	err = h.db.Find(&sps).Error
	if err != nil {
		h.logger.Error("failed to fetch service ports", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch service ports: " + err.Error(),
		})
	}

	for _, sp := range sps {
		err = h.manager.StartTunnel(vm, &sp)
		if err != nil {
			h.logger.Error("failed to start tunnel",
				zap.Error(err),
				zap.String("vm_ip", vm.IP),
				zap.Int("service_port", sp.ServicePort))
		}
	}

	err = tx.Commit().Error
	if err != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to commit transaction: " + err.Error(),
		})
	}

	return c.JSON(http.StatusCreated, models.Response{
		Success: true,
		Data:    vm,
	})
}

func (h *Handler) ListVMs(c echo.Context) error {
	var vms []models.VM
	err := h.db.Preload("Tunnels").Find(&vms).Error
	if err != nil {
		h.logger.Error("failed to fetch VMs", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch VMs: " + err.Error(),
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
			Error:   "Invalid VM ID: " + err.Error(),
		})
	}

	var vm models.VM
	err = h.db.Preload("Tunnels").First(&vm, id).Error
	if err != nil {
		return c.JSON(http.StatusNotFound, models.Response{
			Success: false,
			Error:   "VM not found: " + err.Error(),
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
			Error:   "Invalid VM ID: " + err.Error(),
		})
	}

	var vm models.VM
	err = h.db.First(&vm, id).Error
	if err != nil {
		return c.JSON(http.StatusNotFound, models.Response{
			Success: false,
			Error:   "VM not found: " + err.Error(),
		})
	}

	var sps []models.ServicePort
	err = h.db.Find(&sps).Error
	if err != nil {
		h.logger.Error("failed to fetch service ports", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch service ports: " + err.Error(),
		})
	}

	var req models.CreateVMRequest
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
	if tx.Error != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to start transaction: " + err.Error(),
		})
	}

	needTunnelRestart := vm.IP != req.IP ||
		vm.Port != req.Port ||
		vm.User != req.User ||
		vm.Password != req.Password

	vm.IP = req.IP
	vm.Port = req.Port
	vm.User = req.User
	vm.Password = req.Password
	vm.Description = req.Description

	err = tx.Save(&vm).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to update VM", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to update VM: " + err.Error(),
		})
	}

	if needTunnelRestart && len(sps) > 0 {
		for _, sp := range sps {
			err = h.manager.StopTunnel(vm.ID, sp.ID)
			if err != nil {
				h.logger.Warn("failed to stop tunnel",
					zap.Uint("vm_id", vm.ID),
					zap.Uint("service_port_id", sp.ID),
					zap.Error(err))
			}
		}

		for _, sp := range sps {
			err = h.manager.StartTunnel(&vm, &sp)
			if err != nil {
				h.logger.Error("failed to restart tunnel",
					zap.Error(err),
					zap.String("vm_ip", vm.IP),
					zap.Int("service_port", sp.ServicePort))
			}
		}
	}

	err = tx.Commit().Error
	if err != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to commit transaction: " + err.Error(),
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
			Error:   "Invalid VM ID: " + err.Error(),
		})
	}

	tx := h.db.Begin()
	err = tx.Error
	if tx.Error != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to start transaction: " + err.Error(),
		})
	}

	var vm models.VM
	err = tx.Set("gorm:pessimistic_lock", true).First(&vm, id).Error
	if err != nil {
		tx.Rollback()
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return c.JSON(http.StatusNotFound, models.Response{
				Success: false,
				Error:   "VM not found",
			})
		}
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to lock VM record: " + err.Error(),
		})
	}

	var servicePorts []models.ServicePort
	err = tx.Find(&servicePorts).Error
	if err != nil {
		tx.Rollback()
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch service ports: " + err.Error(),
		})
	}

	for _, sp := range servicePorts {
		err = h.manager.StopTunnel(uint(id), sp.ID)
		if err != nil {
			h.logger.Warn("failed to stop tunnel",
				zap.Uint("vm_id", uint(id)),
				zap.Uint("service_port_id", sp.ID),
				zap.Error(err))
		}
	}

	err = tx.Unscoped().Delete(&vm).Error
	if err != nil {
		tx.Rollback()
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to delete VM: " + err.Error(),
		})
	}

	err = tx.Commit().Error
	if err != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to commit transaction: " + err.Error(),
		})
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    "VM deleted successfully",
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
	if tx.Error != nil {
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

	var vms []models.VM
	err = h.db.Preload("Tunnels").Find(&vms).Error
	if err != nil {
		h.logger.Error("failed to fetch VMs", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch VMs: " + err.Error(),
		})
	}

	for _, vm := range vms {
		err = h.manager.StartTunnel(&vm, sp)
		if err != nil {
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

	err = tx.Commit().Error
	if err != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to commit transaction: " + err.Error(),
		})
	}

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
	if tx.Error != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to start transaction: " + err.Error(),
		})
	}

	var vms []models.VM
	err = h.db.Preload("Tunnels").Find(&vms).Error
	if err != nil {
		h.logger.Error("failed to fetch VMs", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch VMs: " + err.Error(),
		})
	}

	for _, vm := range vms {
		err = h.manager.StopTunnel(vm.ID, sp.ID)
		if err != nil {
			h.logger.Warn("failed to stop existing tunnel",
				zap.String("vm_ip", vm.IP),
				zap.Int("service_port", sp.ServicePort),
				zap.Error(err))
		}
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

	for _, vm := range vms {
		err = h.manager.StartTunnel(&vm, &sp)
		if err != nil {
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

	err = tx.Commit().Error
	if err != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to commit transaction: " + err.Error(),
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
	if tx.Error != nil {
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to start transaction: " + err.Error(),
		})
	}

	var vms []models.VM
	err = h.db.Preload("Tunnels").Find(&vms).Error
	if err != nil {
		h.logger.Error("failed to fetch VMs", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch VMs: " + err.Error(),
		})
	}

	for _, vm := range vms {
		err = h.manager.StopTunnel(vm.ID, sp.ID)
		if err != nil {
			h.logger.Warn("failed to stop tunnel",
				zap.String("vm_ip", vm.IP),
				zap.Int("service_port", sp.ServicePort),
				zap.Error(err))
		}
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

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    "Service port deleted successfully",
	})
}

func (h *Handler) GetStatus(c echo.Context) error {
	var tunnels []models.Tunnel
	err := h.db.Preload("VM").Find(&tunnels).Error
	if err != nil {
		h.logger.Error("failed to fetch tunnel status", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch tunnel status: " + err.Error(),
		})
	}

	var connectedTunnels int
	for _, t := range tunnels {
		if t.Status == "connected" {
			connectedTunnels++
		}
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data: map[string]interface{}{
			"total_tunnels":     len(tunnels),
			"connected_tunnels": connectedTunnels,
			"tunnels":           tunnels,
		},
	})
}

func (h *Handler) GetVMStatus(c echo.Context) error {
	vmID, err := strconv.ParseUint(c.Param("vmId"), 10, 32)
	if err != nil {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "Invalid VM ID: " + err.Error(),
		})
	}

	var vm models.VM
	err = h.db.Preload("Tunnels").First(&vm, vmID).Error
	if err != nil {
		return c.JSON(http.StatusNotFound, models.Response{
			Success: false,
			Error:   "VM not found: " + err.Error(),
		})
	}

	vmWithoutTunnels := vm
	vmWithoutTunnels.Tunnels = nil

	var connectedTunnels int
	for _, t := range vm.Tunnels {
		if t.Status == "connected" {
			connectedTunnels++
		}
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data: map[string]interface{}{
			"vm":                vmWithoutTunnels,
			"total_tunnels":     len(vm.Tunnels),
			"connected_tunnels": connectedTunnels,
			"tunnels":           vm.Tunnels,
		},
	})
}
