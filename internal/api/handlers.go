package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/tunnel"
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
	// DesiredTunnelCount reports how many tunnels should be running. It is
	// taken from the manager rather than counted here, so the state the status
	// is reported against is the one the loop works towards.
	DesiredTunnelCount() (int, error)
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

// sealPassword returns the password of a Host as it is stored. An empty
// password stays empty rather than being sealed: a Host may carry a private key
// and no password, and a sealed empty string is a value the row holds, which
// would have the tunnel offer an empty password to the Host.
func (h *Handler) sealPassword(password string) (string, error) {
	if password == "" {
		return "", nil
	}

	return h.cipher.Encrypt(password)
}

// sealPrivateKey checks the private key and the passphrase that were handed in
// and returns the two as they are stored. An empty key gives two empty values,
// which is a Host registered without one.
//
// The key is parsed here, with the passphrase, so that what reaches the row is
// a key that opens. What comes back for a key that does not is a
// tunnel.KeyError naming what is wrong with it; everything else is a failure of
// this process.
func (h *Handler) sealPrivateKey(keyPEM string, passphrase string) (string, string, error) {
	keyPEM = strings.TrimSpace(keyPEM)
	if keyPEM == "" {
		return "", "", nil
	}

	_, err := tunnel.ParsePrivateKey(keyPEM, passphrase)
	if err != nil {
		return "", "", err
	}

	sealedKey, err := h.cipher.Encrypt(keyPEM)
	if err != nil {
		return "", "", err
	}

	// The passphrase is stored only when there is one. A key that needs none
	// leaves the column empty rather than holding a sealed empty string.
	sealedPassphrase := ""
	if passphrase != "" {
		sealedPassphrase, err = h.cipher.Encrypt(passphrase)
		if err != nil {
			return "", "", err
		}
	}

	return sealedKey, sealedPassphrase, nil
}

// keyRefused answers a key that was not stored. A refusal of the key itself
// carries what is wrong with it, because that is what says which box to go back
// to and what to do about it, and it is built from what was checked rather than
// from anything inside this process. Everything else is answered as a failure
// and written to the log, where the key and the passphrase never appear.
func (h *Handler) keyRefused(c echo.Context, err error, prefix string) error {
	var refused *tunnel.KeyError
	if errors.As(err, &refused) {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   prefix + refused.Error(),
		})
	}

	h.logger.Error("failed to encrypt the private key of the Host", zap.Error(err))

	return c.JSON(http.StatusInternalServerError, models.Response{
		Success: false,
		Error:   "Failed to encrypt the private key",
	})
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

	// A Host that carries neither is one nothing can log in with. It is refused
	// here rather than by a rule on the password field, so that the message can
	// name both ways in and say that either will do.
	if req.Password == "" && strings.TrimSpace(req.PrivateKey) == "" {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error: "The Host was not created: it carries no way to log in. Give a private key, " +
				"a password, or both",
		})
	}

	password, err := h.sealPassword(req.Password)
	if err != nil {
		h.logger.Error("failed to encrypt the password of the Host", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to encrypt the password",
		})
	}

	// The key is read before anything is stored, so that a key that cannot be
	// used is refused while the operator is still looking at the box they
	// pasted it into. A password is only found to be wrong by the Host that
	// refuses it, but a key has a form, and what is wrong with it can be said
	// here.
	privateKey, keyPassphrase, err := h.sealPrivateKey(req.PrivateKey, req.KeyPassphrase)
	if err != nil {
		return h.keyRefused(c, err, "The Host was not created: ")
	}

	tx := h.db.Begin()
	err = tx.Error
	if err != nil {
		h.logger.Error("failed to start the transaction", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to start transaction",
		})
	}

	host := &models.Host{
		IP:            req.IP,
		Port:          req.Port,
		User:          req.User,
		Password:      password,
		PrivateKey:    privateKey,
		KeyPassphrase: keyPassphrase,
		Description:   req.Description,
	}

	err = tx.Create(host).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to create Host", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to create Host",
		})
	}

	err = tx.Commit().Error
	if err != nil {
		h.logger.Error("failed to commit the transaction", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to commit transaction",
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
			Error:   "Failed to fetch Hosts",
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

	// A read that failed is told apart from a row that is not stored, because a
	// database that cannot be reached answered as "no such Host" sends whoever
	// is on call after the client rather than after the database. What went
	// wrong is written to the log alone: the answer would carry the query and
	// the driver of this server to anyone who may make the request, and the id
	// it is about is what the client sent in.
	var host models.Host
	err = h.db.First(&host, id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return c.JSON(http.StatusNotFound, models.Response{
				Success: false,
				Error:   "Host not found",
			})
		}
		h.logger.Error("failed to fetch Host", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch Host",
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

	tx := h.db.Begin()
	err = tx.Error
	if err != nil {
		h.logger.Error("failed to start the transaction", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to start transaction",
		})
	}

	// The row is read inside the transaction, because what is written below is
	// the row that is read here with a few fields replaced. Two requests on the
	// same Host would otherwise both read the old row, and the write that lands
	// second would put back the fields the first one changed.
	//
	// What keeps the two apart is the single database connection that
	// database.NewDatabase opens. SQLite takes one writer at a time, so the
	// pool holds the second transaction until the first has committed, and the
	// read below it sees what the first one wrote. SELECT ... FOR UPDATE used
	// to stand here, but gorm leaves that clause out of SQLite SQL without a
	// word and without an error, so it guarded nothing.
	var host models.Host
	err = tx.First(&host, id).Error
	if err != nil {
		tx.Rollback()
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return c.JSON(http.StatusNotFound, models.Response{
				Success: false,
				Error:   "Host not found",
			})
		}
		h.logger.Error("failed to fetch Host", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch Host",
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
			tx.Rollback()
			h.logger.Error("failed to encrypt the password of the Host", zap.Error(err))
			return c.JSON(http.StatusInternalServerError, models.Response{
				Success: false,
				Error:   "Failed to encrypt the password",
			})
		}
		host.Password = password
	}
	// An empty key box keeps the key that is stored, the way an empty password
	// box keeps the password. A key that is sent replaces both halves at once,
	// because the two were checked as a pair: a new key left beside the
	// passphrase of the old one is a pair nothing checked.
	if strings.TrimSpace(req.PrivateKey) != "" {
		privateKey, keyPassphrase, err := h.sealPrivateKey(req.PrivateKey, req.KeyPassphrase)
		if err != nil {
			tx.Rollback()
			return h.keyRefused(c, err, "The Host was not updated: ")
		}

		host.PrivateKey = privateKey
		host.KeyPassphrase = keyPassphrase
	} else if req.KeyPassphrase != "" {
		tx.Rollback()
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error: "The Host was not updated: a passphrase was sent without a private key. " +
				"The two are checked together, so send the key along with it",
		})
	}
	if req.Description != "" {
		host.Description = req.Description
	}
	if req.Enabled != nil {
		host.Enabled = *req.Enabled
	}

	err = tx.Save(&host).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to update Host", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to update Host",
		})
	}

	err = tx.Commit().Error
	if err != nil {
		h.logger.Error("failed to commit the transaction", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to commit transaction",
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

	tx := h.db.Begin()
	err = tx.Error
	if err != nil {
		h.logger.Error("failed to start the transaction", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to start transaction",
		})
	}

	// Read inside the transaction as in UpdateHost, so that the answer and the
	// delete agree: an update of the same Host either lands before the read,
	// and is deleted with the row, or waits for the pool and finds the row gone.
	var host models.Host
	err = tx.First(&host, id).Error
	if err != nil {
		tx.Rollback()
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return c.JSON(http.StatusNotFound, models.Response{
				Success: false,
				Error:   "Host not found",
			})
		}
		h.logger.Error("failed to fetch Host", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch Host",
		})
	}

	err = tx.Delete(&host).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to delete Host", zap.Error(err), zap.Uint64("host_id", id))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to delete Host",
		})
	}

	err = tx.Commit().Error
	if err != nil {
		h.logger.Error("failed to commit the transaction", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to commit transaction",
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
		h.logger.Error("failed to start the transaction", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to start transaction",
		})
	}

	err = tx.Create(sp).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to create service port", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to create service port",
		})
	}

	err = tx.Commit().Error
	if err != nil {
		h.logger.Error("failed to commit the transaction", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to commit transaction",
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
			Error:   "Invalid service port ID: " + err.Error(),
		})
	}

	var sp models.ServicePort
	err = h.db.First(&sp, id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return c.JSON(http.StatusNotFound, models.Response{
				Success: false,
				Error:   "Service port not found",
			})
		}
		h.logger.Error("failed to fetch service port", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch service port",
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

	// The body is read before the transaction is opened, because reading it
	// waits on the client and the lock taken below is held until the commit.
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
		h.logger.Error("failed to start the transaction", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to start transaction",
		})
	}

	// Read inside the transaction for the reason UpdateHost is: the write below
	// carries the fields this read brought in.
	var sp models.ServicePort
	err = tx.First(&sp, id).Error
	if err != nil {
		tx.Rollback()
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return c.JSON(http.StatusNotFound, models.Response{
				Success: false,
				Error:   "Service port not found",
			})
		}
		h.logger.Error("failed to fetch service port", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch service port",
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
			Error:   "Failed to update service port",
		})
	}

	err = tx.Commit().Error
	if err != nil {
		h.logger.Error("failed to commit the transaction", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to commit transaction",
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

	tx := h.db.Begin()
	err = tx.Error
	if err != nil {
		h.logger.Error("failed to start the transaction", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to start transaction",
		})
	}

	// Read inside the transaction as in DeleteHost, so that an update of the
	// same service port and this delete do not both act on the row they read.
	var sp models.ServicePort
	err = tx.First(&sp, id).Error
	if err != nil {
		tx.Rollback()
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return c.JSON(http.StatusNotFound, models.Response{
				Success: false,
				Error:   "Service port not found",
			})
		}
		h.logger.Error("failed to fetch service port", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch service port",
		})
	}

	err = tx.Delete(&sp).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to delete service port", zap.Error(err), zap.Uint64("service_port_id", id))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to delete service port",
		})
	}

	err = tx.Commit().Error
	if err != nil {
		h.logger.Error("failed to commit the transaction", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to commit transaction",
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
		h.logger.Error("failed to fetch the tunnel status", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch tunnel status",
		})
	}

	// A count that is missing is an error rather than a field left out: the
	// answer without it reads as if every tunnel that should run does, which
	// is the very thing the field is there to show.
	desiredTunnels, err := h.manager.DesiredTunnelCount()
	if err != nil {
		h.logger.Error("failed to count the tunnels that should be running", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to count the tunnels that should be running",
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
			"desired_tunnels":   desiredTunnels,
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
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return c.JSON(http.StatusNotFound, models.Response{
				Success: false,
				Error:   "Host not found",
			})
		}
		h.logger.Error("failed to fetch Host", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch Host",
		})
	}

	tunnels, err := h.manager.GetHostTunnels(uint(hostID))
	if err != nil {
		h.logger.Error("failed to fetch the tunnel status of the Host", zap.Error(err),
			zap.Uint64("host_id", hostID))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to fetch tunnel status",
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
