package api

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"github.com/jollaman999/tunnel-manager/internal/logid"
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

// pageSizes are the sizes a list request may ask a page in. The size is taken
// from this list rather than as any number the request carries, because a free
// size is a way to ask for every row in one answer, which is the load the
// paging is here to keep off the database and off the screen.
var pageSizes = []int{10, 20, 30, 50, 100}

// defaultPageSize is the size a request that names none is answered in.
const defaultPageSize = 10

// listPage is the page of a list a request asked for.
type listPage struct {
	number int
	size   int
}

// offset is the row the page begins at.
func (p listPage) offset() int {
	return (p.number - 1) * p.size
}

// fitTo moves the page onto the rows that are there and hands back the page the
// request is answered with.
//
// A page past the last one is not refused. Rows are deleted while a screen is
// open, so the page a client sits on can be gone by the time it asks again, and
// an error there would leave that screen with nothing where the rows that are
// left belong. It is answered with the last page instead. A list with no rows
// at all is page 1, which is the empty first page.
func (p listPage) fitTo(total int64) listPage {
	last := (total + int64(p.size) - 1) / int64(p.size)
	if last < 1 {
		last = 1
	}

	if int64(p.number) > last {
		p.number = int(last)
	}

	return p
}

// listPageOf is what a list answers with. The rows of the page carry how many
// rows there are in all and which page of which size these are, because an
// array on its own says nothing about what is not in it and a client cannot
// tell a short last page from the whole list.
type listPageOf struct {
	Items interface{} `json:"items"`
	Total int64       `json:"total"`
	Page  int         `json:"page"`
	Size  int         `json:"size"`
}

// readListPage reads the page and the size off the query string of a list
// request. A page below 1 is read as 1; what becomes of a page above the last
// one is decided by fitTo, once the rows have been counted.
//
// A size that is not one of pageSizes is refused rather than brought into
// range, and the refusal names the sizes it takes, so that no request is
// answered with a page of a size it did not ask for.
//
// The two values it can be sent wrong are two refusals rather than one with the
// reason written into it. What is wrong with a page is not what is wrong with a
// size, and a screen that shows either in its own language has to be able to
// tell them apart. The value is quoted where it is handed over, because the
// English sentence quotes it and the quoting is what marks where it ends.
func readListPage(c echo.Context) (listPage, *refusal) {
	page := listPage{number: 1, size: defaultPageSize}

	raw := c.QueryParam("page")
	if raw != "" {
		number, err := strconv.Atoi(raw)
		if err != nil {
			return listPage{}, refuse(http.StatusBadRequest, errListPageNotANumber,
				errorArgs{"page": strconv.Quote(raw)})
		}

		if number < 1 {
			number = 1
		}

		page.number = number
	}

	raw = c.QueryParam("size")
	if raw != "" {
		size, err := strconv.Atoi(raw)
		if err != nil || !isPageSize(size) {
			return listPage{}, refuse(http.StatusBadRequest, errListSizeUnsupported,
				errorArgs{"sizes": pageSizeList(), "size": strconv.Quote(raw)})
		}

		page.size = size
	}

	return page, nil
}

// isPageSize reports whether a size is one of the sizes a page is served in.
func isPageSize(size int) bool {
	for _, allowed := range pageSizes {
		if size == allowed {
			return true
		}
	}

	return false
}

// pageSizeList is pageSizes as the refusal above names them, built from the
// list itself so that a size added to it is a size the message says.
func pageSizeList() string {
	sizes := make([]string, 0, len(pageSizes))
	for _, size := range pageSizes {
		sizes = append(sizes, strconv.Itoa(size))
	}

	return strings.Join(sizes, ", ")
}

// badListPage answers a page or a size the request cannot be served with. It
// carries what is wrong with it: the request is the thing that is wrong, and
// the client is the one that can put it right.
//
// readListPage hands back the refusal already named, rather than an error this
// would have to read the text of, so that the two ways a page can be wrong stay
// two refusals on the way out.
func badListPage(c echo.Context, refused *refusal) error {
	return refused.answer(c)
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
// The code is handed in rather than a prefix, because the sentence a create
// and an update are refused with differ in more than a language that is not
// this one can be expected to put back together.
func (h *Handler) keyRefused(c echo.Context, err error, code errorCode) error {
	var refused *tunnel.KeyError
	if errors.As(err, &refused) {
		return failure(c, http.StatusBadRequest, code, errorArgs{"reason": refused.Error()})
	}

	h.logger.Error("failed to encrypt the private key of the Host",
		logid.HostPrivateKeyEncryptFailed.Field(),
		zap.Error(err))

	return failure(c, http.StatusInternalServerError, errHostKeyEncryptFailed)
}

// wantsAssignments is what a create request asked about the assignments it
// makes. A request that did not mention the field asks for them: every Host
// carried every service port before the assignments were rows of their own, a
// row with none runs no tunnel, and a client written before the field existed
// sends nothing. The field is a pointer so that "no assignments" can be told
// from "did not say", which is the whole of why it is one.
func wantsAssignments(asked *bool) bool {
	if asked == nil {
		return true
	}

	return *asked
}

func (h *Handler) CreateHost(c echo.Context) error {
	var req models.CreateHostRequest
	err := c.Bind(&req)
	if err != nil {
		return failure(c, http.StatusBadRequest, errRequestBodyInvalid, errorArgs{"reason": err.Error()})
	}

	err = c.Validate(&req)
	if err != nil {
		return failure(c, http.StatusBadRequest, errRequestValidationFailed, errorArgs{"reason": err.Error()})
	}

	// A Host that carries neither is one nothing can log in with. It is refused
	// here rather than by a rule on the password field, so that the message can
	// name both ways in and say that either will do.
	if req.Password == "" && strings.TrimSpace(req.PrivateKey) == "" {
		return failure(c, http.StatusBadRequest, errHostCreateNoLogin)
	}

	password, err := h.sealPassword(req.Password)
	if err != nil {
		h.logger.Error("failed to encrypt the password of the Host", logid.HostPasswordEncryptFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errHostPasswordEncryptFailed)
	}

	// The key is read before anything is stored, so that a key that cannot be
	// used is refused while the operator is still looking at the box they
	// pasted it into. A password is only found to be wrong by the Host that
	// refuses it, but a key has a form, and what is wrong with it can be said
	// here.
	privateKey, keyPassphrase, err := h.sealPrivateKey(req.PrivateKey, req.KeyPassphrase)
	if err != nil {
		return h.keyRefused(c, err, errHostCreateKeyRefused)
	}

	tx := h.db.Begin()
	err = tx.Error
	if err != nil {
		h.logger.Error("failed to start the transaction", logid.DatabaseTransactionStartFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionBeginFailed)
	}

	// A Host that does not say is enabled. Saying nothing is how every Host was
	// registered before the field could be sent, and a Host registered to be
	// used is the ordinary case. The field is a pointer so that a Host asked
	// for as disabled can be told from one that did not mention it at all,
	// which a plain bool cannot say.
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	host := &models.Host{
		IP:            req.IP,
		Port:          req.Port,
		User:          req.User,
		Password:      password,
		PrivateKey:    privateKey,
		KeyPassphrase: keyPassphrase,
		Description:   req.Description,
		Enabled:       enabled,
	}

	err = tx.Create(host).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to create Host", logid.HostCreateFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errHostCreateFailed)
	}

	// The assignments are written in the transaction that wrote the Host, so
	// that the two land together. A Host stored without them is one that shows
	// on the screen and carries nothing, and nothing later would notice.
	//
	// The service ports are read inside the transaction as well, for the reason
	// the update handlers read their row there: what is assigned is what is
	// stored at the moment the Host is written.
	if wantsAssignments(req.AssignAllServicePorts) {
		var spIDs []uint

		err = tx.Model(&models.ServicePort{}).Pluck("id", &spIDs).Error
		if err != nil {
			tx.Rollback()
			h.logger.Error("failed to read the service ports to assign to a new Host",
				logid.HostNewServicePortsReadFailed.Field(),
				zap.Error(err))
			return failure(c, http.StatusInternalServerError, errHostCreateServicePortsRead)
		}

		for _, spID := range spIDs {
			err = tx.Create(&models.HostServicePort{HostID: host.ID, SPID: spID}).Error
			if err != nil {
				tx.Rollback()
				h.logger.Error("failed to assign a service port to a new Host",
					logid.HostNewServicePortAssignFailed.Field(),
					zap.Error(err), zap.Uint("host_id", host.ID), zap.Uint("service_port_id", spID))
				return failure(c, http.StatusInternalServerError, errHostCreateServicePortsStore)
			}
		}
	}

	err = tx.Commit().Error
	if err != nil {
		h.logger.Error("failed to commit the transaction", logid.DatabaseTransactionCommitFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionCommitFailed)
	}

	h.manager.WakeReconcile()

	return c.JSON(http.StatusCreated, models.Response{
		Success: true,
		Data:    hostViewOf(*host),
	})
}

// ListHosts answers one page of the Hosts, in the order they were registered
// in. The order is stated rather than left to the database, because LIMIT and
// OFFSET cut a page out of an order: rows handed back in a different order from
// one read to the next would put one Host on two pages and another on none. The
// id is what that order is taken from, since it is given out once, never
// changes, and no two Hosts share one.
func (h *Handler) ListHosts(c echo.Context) error {
	page, refused := readListPage(c)
	if refused != nil {
		return badListPage(c, refused)
	}

	// The rows are counted by the database rather than read and measured here.
	// Reading every row to find out how many there are is the work the paging
	// is here to avoid.
	var total int64
	err := h.db.Model(&models.Host{}).Count(&total).Error
	if err != nil {
		h.logger.Error("failed to count the Hosts", logid.HostCountFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errHostListFailed)
	}

	page = page.fitTo(total)

	var hosts []models.Host
	err = h.db.Order("id").Limit(page.size).Offset(page.offset()).Find(&hosts).Error
	if err != nil {
		h.logger.Error("failed to fetch Hosts", logid.HostListFetchFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errHostListFailed)
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data: listPageOf{
			Items: hostViewsOf(hosts),
			Total: total,
			Page:  page.number,
			Size:  page.size,
		},
	})
}

func (h *Handler) GetHost(c echo.Context) error {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		return failure(c, http.StatusBadRequest, errHostIDInvalid, errorArgs{"reason": err.Error()})
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
			return failure(c, http.StatusNotFound, errHostNotFound)
		}
		h.logger.Error("failed to fetch Host", logid.HostFetchFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errHostFetchFailed)
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    hostViewOf(host),
	})
}

func (h *Handler) UpdateHost(c echo.Context) error {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		return failure(c, http.StatusBadRequest, errHostIDInvalid, errorArgs{"reason": err.Error()})
	}

	var req models.UpdateHostRequest
	err = c.Bind(&req)
	if err != nil {
		return failure(c, http.StatusBadRequest, errRequestBodyInvalid, errorArgs{"reason": err.Error()})
	}

	err = c.Validate(&req)
	if err != nil {
		return failure(c, http.StatusBadRequest, errRequestValidationFailed, errorArgs{"reason": err.Error()})
	}

	tx := h.db.Begin()
	err = tx.Error
	if err != nil {
		h.logger.Error("failed to start the transaction", logid.DatabaseTransactionStartFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionBeginFailed)
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
			return failure(c, http.StatusNotFound, errHostNotFound)
		}
		h.logger.Error("failed to fetch Host", logid.HostFetchFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errHostFetchFailed)
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
			h.logger.Error("failed to encrypt the password of the Host", logid.HostPasswordEncryptFailed.Field(), zap.Error(err))
			return failure(c, http.StatusInternalServerError, errHostPasswordEncryptFailed)
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
			return h.keyRefused(c, err, errHostUpdateKeyRefused)
		}

		host.PrivateKey = privateKey
		host.KeyPassphrase = keyPassphrase
	} else if req.KeyPassphrase != "" {
		tx.Rollback()
		return failure(c, http.StatusBadRequest, errHostUpdatePassphraseAlone)
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
		h.logger.Error("failed to update Host", logid.HostUpdateFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errHostUpdateFailed)
	}

	err = tx.Commit().Error
	if err != nil {
		h.logger.Error("failed to commit the transaction", logid.DatabaseTransactionCommitFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionCommitFailed)
	}

	h.manager.WakeReconcile()

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    hostViewOf(host),
	})
}

func (h *Handler) DeleteHost(c echo.Context) error {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		return failure(c, http.StatusBadRequest, errHostIDInvalid, errorArgs{"reason": err.Error()})
	}

	tx := h.db.Begin()
	err = tx.Error
	if err != nil {
		h.logger.Error("failed to start the transaction", logid.DatabaseTransactionStartFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionBeginFailed)
	}

	// Read inside the transaction as in UpdateHost, so that the answer and the
	// delete agree: an update of the same Host either lands before the read,
	// and is deleted with the row, or waits for the pool and finds the row gone.
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

	err = tx.Delete(&host).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to delete Host", logid.HostDeleteFailed.Field(), zap.Error(err), zap.Uint64("host_id", id))
		return failure(c, http.StatusInternalServerError, errHostDeleteFailed)
	}

	// The assignments of this Host go with it, in the same transaction as the
	// row itself: half of the two left behind would be an assignment naming a
	// Host that is gone. A reconcile pass passes such a row over, so no tunnel
	// comes of it, but the next Host to be given this identifier would inherit
	// the service ports of the one that was deleted.
	err = tx.Where("host_id = ?", host.ID).Delete(&models.HostServicePort{}).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to delete the service port assignments of a Host",
			logid.HostServicePortAssignmentsDeleteFailed.Field(),
			zap.Error(err), zap.Uint64("host_id", id))
		return failure(c, http.StatusInternalServerError, errHostDeleteFailed)
	}

	err = tx.Commit().Error
	if err != nil {
		h.logger.Error("failed to commit the transaction", logid.DatabaseTransactionCommitFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionCommitFailed)
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
		return failure(c, http.StatusBadRequest, errRequestBodyInvalid, errorArgs{"reason": err.Error()})
	}

	err = c.Validate(&req)
	if err != nil {
		return failure(c, http.StatusBadRequest, errRequestValidationFailed, errorArgs{"reason": err.Error()})
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
		h.logger.Error("failed to start the transaction", logid.DatabaseTransactionStartFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionBeginFailed)
	}

	err = tx.Create(sp).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to create service port", logid.ServicePortCreateFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errServicePortCreateFailed)
	}

	// The other half of what CreateHost does, and in the transaction that wrote
	// the row for the same reason: a service port stored with no Host carrying
	// it is one nothing forwards.
	//
	// A Host that is disabled is assigned it too. Which Hosts run tunnels is
	// decided where they are reconciled, and a Host skipped here would come
	// back from being enabled without this service port.
	if wantsAssignments(req.AssignToAllHosts) {
		var hostIDs []uint

		err = tx.Model(&models.Host{}).Pluck("id", &hostIDs).Error
		if err != nil {
			tx.Rollback()
			h.logger.Error("failed to read the Hosts to assign a new service port to",
				logid.ServicePortHostsReadFailed.Field(),
				zap.Error(err))
			return failure(c, http.StatusInternalServerError, errServicePortCreateHostsRead)
		}

		for _, hostID := range hostIDs {
			err = tx.Create(&models.HostServicePort{HostID: hostID, SPID: sp.ID}).Error
			if err != nil {
				tx.Rollback()
				h.logger.Error("failed to assign a new service port to a Host",
					logid.ServicePortHostAssignFailed.Field(),
					zap.Error(err), zap.Uint("host_id", hostID), zap.Uint("service_port_id", sp.ID))
				return failure(c, http.StatusInternalServerError, errServicePortCreateHostsStore)
			}
		}
	}

	err = tx.Commit().Error
	if err != nil {
		h.logger.Error("failed to commit the transaction", logid.DatabaseTransactionCommitFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionCommitFailed)
	}

	// The row is stored, which is what the answer reports. Whether a tunnel can
	// be built for it is answered by the status of the tunnels, not here.
	h.manager.WakeReconcile()

	return c.JSON(http.StatusCreated, models.Response{
		Success: true,
		Data:    sp,
	})
}

// ListServicePorts answers one page of the service ports, ordered by id for the
// reason ListHosts is.
func (h *Handler) ListServicePorts(c echo.Context) error {
	page, refused := readListPage(c)
	if refused != nil {
		return badListPage(c, refused)
	}

	var total int64
	err := h.db.Model(&models.ServicePort{}).Count(&total).Error
	if err != nil {
		h.logger.Error("failed to count the service ports", logid.ServicePortCountFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errServicePortListFailed)
	}

	page = page.fitTo(total)

	var sps []models.ServicePort
	err = h.db.Order("id").Limit(page.size).Offset(page.offset()).Find(&sps).Error
	if err != nil {
		h.logger.Error("failed to fetch service ports", logid.ServicePortListFetchFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errServicePortListFailed)
	}

	if sps == nil {
		sps = []models.ServicePort{}
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data: listPageOf{
			Items: sps,
			Total: total,
			Page:  page.number,
			Size:  page.size,
		},
	})
}

func (h *Handler) GetServicePort(c echo.Context) error {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		return failure(c, http.StatusBadRequest, errServicePortIDInvalid, errorArgs{"reason": err.Error()})
	}

	var sp models.ServicePort
	err = h.db.First(&sp, id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return failure(c, http.StatusNotFound, errServicePortNotFound)
		}
		h.logger.Error("failed to fetch service port", logid.ServicePortFetchFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errServicePortFetchFailed)
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    sp,
	})
}

func (h *Handler) UpdateServicePort(c echo.Context) error {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		return failure(c, http.StatusBadRequest, errServicePortIDInvalid, errorArgs{"reason": err.Error()})
	}

	// The body is read before the transaction is opened, because reading it
	// waits on the client and the lock taken below is held until the commit.
	var req models.CreateServicePortRequest
	err = c.Bind(&req)
	if err != nil {
		return failure(c, http.StatusBadRequest, errRequestBodyInvalid, errorArgs{"reason": err.Error()})
	}

	err = c.Validate(&req)
	if err != nil {
		return failure(c, http.StatusBadRequest, errRequestValidationFailed, errorArgs{"reason": err.Error()})
	}

	tx := h.db.Begin()
	err = tx.Error
	if err != nil {
		h.logger.Error("failed to start the transaction", logid.DatabaseTransactionStartFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionBeginFailed)
	}

	// Read inside the transaction for the reason UpdateHost is: the write below
	// carries the fields this read brought in.
	var sp models.ServicePort
	err = tx.First(&sp, id).Error
	if err != nil {
		tx.Rollback()
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return failure(c, http.StatusNotFound, errServicePortNotFound)
		}
		h.logger.Error("failed to fetch service port", logid.ServicePortFetchFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errServicePortFetchFailed)
	}

	sp.ServiceIP = req.ServiceIP
	sp.ServicePort = req.ServicePort
	sp.LocalPort = req.LocalPort
	sp.Description = req.Description

	err = tx.Save(&sp).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to update service port", logid.ServicePortUpdateFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errServicePortUpdateFailed)
	}

	err = tx.Commit().Error
	if err != nil {
		h.logger.Error("failed to commit the transaction", logid.DatabaseTransactionCommitFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionCommitFailed)
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
		return failure(c, http.StatusBadRequest, errServicePortIDInvalid, errorArgs{"reason": err.Error()})
	}

	tx := h.db.Begin()
	err = tx.Error
	if err != nil {
		h.logger.Error("failed to start the transaction", logid.DatabaseTransactionStartFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionBeginFailed)
	}

	// Read inside the transaction as in DeleteHost, so that an update of the
	// same service port and this delete do not both act on the row they read.
	var sp models.ServicePort
	err = tx.First(&sp, id).Error
	if err != nil {
		tx.Rollback()
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return failure(c, http.StatusNotFound, errServicePortNotFound)
		}
		h.logger.Error("failed to fetch service port", logid.ServicePortFetchFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errServicePortFetchFailed)
	}

	err = tx.Delete(&sp).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to delete service port",
			logid.ServicePortDeleteFailed.Field(),
			zap.Error(err),
			zap.Uint64("service_port_id", id))
		return failure(c, http.StatusInternalServerError, errServicePortDeleteFailed)
	}

	// The assignments of this service port go with it, as in DeleteHost: a row
	// naming a service port that is gone builds no tunnel and would hand this
	// identifier, once it is given out again, to every Host that carried the
	// old one.
	err = tx.Where("sp_id = ?", sp.ID).Delete(&models.HostServicePort{}).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to delete the Host assignments of a service port",
			logid.ServicePortHostAssignmentsDeleteFailed.Field(),
			zap.Error(err), zap.Uint64("service_port_id", id))
		return failure(c, http.StatusInternalServerError, errServicePortDeleteFailed)
	}

	err = tx.Commit().Error
	if err != nil {
		h.logger.Error("failed to commit the transaction", logid.DatabaseTransactionCommitFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionCommitFailed)
	}

	h.manager.WakeReconcile()

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    "Service port deleted successfully",
	})
}

// hostServicePortItem is one row of the list the assignment screen of a Host
// draws: a service port, with whether this Host is assigned to carry it. The
// service ports that are not assigned are carried as well, because a list of
// the assigned ones alone leaves the screen with nothing to offer as the next
// assignment.
type hostServicePortItem struct {
	models.ServicePort
	Assigned bool `json:"assigned"`
}

// ListHostServicePorts answers one page of the service ports with the
// assignments of one Host laid over them.
//
// The page is taken over the service ports and not over the assignments, and
// ordered by id as ListServicePorts is, so that a row sits on the same page of
// both lists whether this Host carries it or not.
func (h *Handler) ListHostServicePorts(c echo.Context) error {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		return failure(c, http.StatusBadRequest, errHostIDInvalid, errorArgs{"reason": err.Error()})
	}

	page, refused := readListPage(c)
	if refused != nil {
		return badListPage(c, refused)
	}

	// The Host is read first, so that a request naming one that is not there is
	// answered as such. Without this read the answer would be every service
	// port with nothing assigned, which is what a Host that carries none looks
	// like, and the screen could not tell the two apart.
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
	err = h.db.Model(&models.ServicePort{}).Count(&total).Error
	if err != nil {
		h.logger.Error("failed to count the service ports", logid.ServicePortCountFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errServicePortListFailed)
	}

	page = page.fitTo(total)

	var sps []models.ServicePort
	err = h.db.Order("id").Limit(page.size).Offset(page.offset()).Find(&sps).Error
	if err != nil {
		h.logger.Error("failed to fetch service ports", logid.ServicePortListFetchFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errServicePortListFailed)
	}

	// Only the assignments of the rows on this page are read. The table holds a
	// row per Host and service port, and the page says nothing about the ones
	// it does not carry, so reading the rest is work no answer is built from.
	ids := make([]uint, 0, len(sps))
	for _, sp := range sps {
		ids = append(ids, sp.ID)
	}

	assigned := make(map[uint]bool, len(ids))
	if len(ids) > 0 {
		var spIDs []uint
		err = h.db.Model(&models.HostServicePort{}).
			Where("host_id = ? AND sp_id IN ?", host.ID, ids).Pluck("sp_id", &spIDs).Error
		if err != nil {
			h.logger.Error("failed to fetch the service port assignments of a Host",
				logid.HostServicePortAssignmentsFetchFailed.Field(),
				zap.Error(err), zap.Uint64("host_id", id))
			return failure(c, http.StatusInternalServerError, errServicePortListFailed)
		}

		for _, spID := range spIDs {
			assigned[spID] = true
		}
	}

	// An empty page is an array and not null, for the reason ListHosts says.
	items := make([]hostServicePortItem, 0, len(sps))
	for _, sp := range sps {
		items = append(items, hostServicePortItem{ServicePort: sp, Assigned: assigned[sp.ID]})
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data: listPageOf{
			Items: items,
			Total: total,
			Page:  page.number,
			Size:  page.size,
		},
	})
}

// hostServicePortChange is the change a request makes to the assignments of one
// Host: the service ports to give it, and the ones to take away.
//
// It is a change and not the whole set of service ports the Host is to carry,
// and that is the point of the shape. The list it is made from is served a page
// at a time, so a screen holds one page and knows nothing of the rows on the
// pages it has not read. A whole set sent from there would name what is on that
// page alone, and every assignment outside it would be deleted by a request
// that was meant to tick one box. A change can only say what was touched.
type hostServicePortChange struct {
	Add    []uint `json:"add"`
	Remove []uint `json:"remove"`
}

// hostServicePortChanged is what the change did: how many assignments it wrote
// and how many it removed. The two are counted over the rows and not over the
// request, because a service port already assigned is asked for again without
// being written, and one that is not assigned is removed without a row going.
type hostServicePortChanged struct {
	Added   int `json:"added"`
	Removed int `json:"removed"`
}

// UpdateHostServicePorts adds and removes assignments of one Host.
//
// The whole of the change lands or none of it does. The reconcile loop reads
// these rows to decide which tunnels to run, so a change that landed in part
// would leave the installation carrying traffic over a set of tunnels that no
// request asked for, and nothing later would put it right.
func (h *Handler) UpdateHostServicePorts(c echo.Context) error {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		return failure(c, http.StatusBadRequest, errHostIDInvalid, errorArgs{"reason": err.Error()})
	}

	// The body is read before the transaction is opened, for the reason
	// UpdateHost gives: reading it waits on the client, and the transaction
	// holds a lock until it is committed.
	var req hostServicePortChange
	err = c.Bind(&req)
	if err != nil {
		return failure(c, http.StatusBadRequest, errRequestBodyInvalid, errorArgs{"reason": err.Error()})
	}

	add := sortedIDs(req.Add)
	remove := sortedIDs(req.Remove)

	// A service port named on both sides is refused rather than settled here.
	// Which of the two would win is a guess at what the request meant, and what
	// it decides is whether a tunnel to that service runs.
	both := idsIn(add, remove)
	if len(both) > 0 {
		return failure(c, http.StatusBadRequest, errAssignmentAddAndRemove, errorArgs{"ids": idList(both)})
	}

	tx := h.db.Begin()
	err = tx.Error
	if err != nil {
		h.logger.Error("failed to start the transaction", logid.DatabaseTransactionStartFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionBeginFailed)
	}

	// Read inside the transaction as in UpdateHost: the rows written below name
	// this Host, and a Host deleted between the read and them would be left
	// with assignments after it is gone.
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

	// Every service port to be added is looked for before anything is written,
	// inside the same transaction, so that a request naming one that is not
	// stored changes nothing rather than landing the ones that were read first.
	// The refusal says which identifiers they are: the request is the thing
	// that is wrong and the client is the one that can put it right.
	if len(add) > 0 {
		var stored []uint
		err = tx.Model(&models.ServicePort{}).Where("id IN ?", add).Pluck("id", &stored).Error
		if err != nil {
			tx.Rollback()
			h.logger.Error("failed to fetch service ports", logid.ServicePortListFetchFailed.Field(), zap.Error(err))
			return failure(c, http.StatusInternalServerError, errServicePortListFailed)
		}

		missing := idsNotIn(add, stored)
		if len(missing) > 0 {
			tx.Rollback()
			return failure(c, http.StatusBadRequest, errAssignmentServicePortsMissing, errorArgs{"ids": idList(missing)})
		}
	}

	// An assignment that is already there is left where it is rather than
	// refused. The screen sends what it was told to change, and a box that was
	// ticked while the request was on its way is the state it asked for.
	added := 0
	if len(add) > 0 {
		var carried []uint
		err = tx.Model(&models.HostServicePort{}).
			Where("host_id = ? AND sp_id IN ?", host.ID, add).Pluck("sp_id", &carried).Error
		if err != nil {
			tx.Rollback()
			h.logger.Error("failed to fetch the service port assignments of a Host",
				logid.HostServicePortAssignmentsFetchFailed.Field(),
				zap.Error(err), zap.Uint64("host_id", id))
			return failure(c, http.StatusInternalServerError, errAssignmentUpdateFailed)
		}

		for _, spID := range idsNotIn(add, carried) {
			err = tx.Create(&models.HostServicePort{HostID: host.ID, SPID: spID}).Error
			if err != nil {
				tx.Rollback()
				h.logger.Error("failed to assign a service port to a Host",
					logid.HostServicePortAssignFailed.Field(),
					zap.Error(err), zap.Uint64("host_id", id), zap.Uint("service_port_id", spID))
				return failure(c, http.StatusInternalServerError, errAssignmentUpdateFailed)
			}

			added++
		}
	}

	// A service port that is not assigned is removed without a row going, and
	// that is not an error either: the state the request asked for is the state
	// it is left in.
	removed := 0
	if len(remove) > 0 {
		result := tx.Where("host_id = ? AND sp_id IN ?", host.ID, remove).Delete(&models.HostServicePort{})
		if result.Error != nil {
			tx.Rollback()
			h.logger.Error("failed to remove the service ports of a Host",
				logid.HostServicePortsRemoveFailed.Field(),
				zap.Error(result.Error), zap.Uint64("host_id", id))
			return failure(c, http.StatusInternalServerError, errAssignmentUpdateFailed)
		}

		removed = int(result.RowsAffected)
	}

	err = tx.Commit().Error
	if err != nil {
		h.logger.Error("failed to commit the transaction", logid.DatabaseTransactionCommitFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionCommitFailed)
	}

	// The loop is woken once the transaction is committed, as everywhere here,
	// and only when a row was written: a change that left the table as it was
	// leaves the tunnels the loop wants as they were, so there is nothing for a
	// pass to do about it.
	if added > 0 || removed > 0 {
		h.manager.WakeReconcile()
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    hostServicePortChanged{Added: added, Removed: removed},
	})
}

// sortedIDs is the identifiers of a list, each of them once and in order, so
// that what is written and what a refusal names do not follow the order the
// request happened to be written in.
func sortedIDs(ids []uint) []uint {
	seen := make(map[uint]bool, len(ids))
	unique := make([]uint, 0, len(ids))

	for _, id := range ids {
		if seen[id] {
			continue
		}

		seen[id] = true
		unique = append(unique, id)
	}

	sort.Slice(unique, func(i, j int) bool { return unique[i] < unique[j] })

	return unique
}

// idsIn is the identifiers of ids that are in other, in the order of ids.
func idsIn(ids, other []uint) []uint {
	return idsOf(ids, other, true)
}

// idsNotIn is the identifiers of ids that are not in other, in the order of ids.
func idsNotIn(ids, other []uint) []uint {
	return idsOf(ids, other, false)
}

// idsOf is the identifiers of ids whose being in other is what is wanted.
func idsOf(ids, other []uint, wanted bool) []uint {
	in := make(map[uint]bool, len(other))
	for _, id := range other {
		in[id] = true
	}

	found := make([]uint, 0, len(ids))
	for _, id := range ids {
		if in[id] == wanted {
			found = append(found, id)
		}
	}

	return found
}

// idList is identifiers as a refusal names them.
func idList(ids []uint) string {
	text := make([]string, 0, len(ids))
	for _, id := range ids {
		text = append(text, strconv.FormatUint(uint64(id), 10))
	}

	return strings.Join(text, ", ")
}

// GetStatus answers the counts of the installation along with one page of the
// tunnel rows.
//
// The three counts are over every row and not over the page. They are what the
// status screen says the installation is doing, and counted over a page they
// would follow the page size around: total_tunnels would read as the size of
// the page, and a page without a connected tunnel on it would say that nothing
// is connected while the tunnels carry traffic.
//
// The page itself is ordered by the two columns the row is identified by, host
// first, so that the order is one the database states rather than one it
// happens to return. See ListHosts for what an order that is not stated does to
// LIMIT and OFFSET.
func (h *Handler) GetStatus(c echo.Context) error {
	page, refused := readListPage(c)
	if refused != nil {
		return badListPage(c, refused)
	}

	// A count that is missing is an error rather than a field left out: the
	// answer without it reads as if every tunnel that should run does, which
	// is the very thing the field is there to show.
	desiredTunnels, err := h.manager.DesiredTunnelCount()
	if err != nil {
		h.logger.Error("failed to count the tunnels that should be running",
			logid.StatusTunnelsToRunCountFailed.Field(),
			zap.Error(err))
		return failure(c, http.StatusInternalServerError, errStatusDesiredCountFailed)
	}

	var totalTunnels int64
	err = h.db.Model(&models.Tunnel{}).Count(&totalTunnels).Error
	if err != nil {
		h.logger.Error("failed to count the tunnel rows", logid.StatusTunnelRowsCountFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errStatusFetchFailed)
	}

	var connectedTunnels int64
	err = h.db.Model(&models.Tunnel{}).Where("status = ?", "connected").Count(&connectedTunnels).Error
	if err != nil {
		h.logger.Error("failed to count the connected tunnels",
			logid.StatusConnectedTunnelsCountFailed.Field(),
			zap.Error(err))
		return failure(c, http.StatusInternalServerError, errStatusFetchFailed)
	}

	page = page.fitTo(totalTunnels)

	var tunnels []models.Tunnel
	err = h.db.Order("host_id, sp_id").Limit(page.size).Offset(page.offset()).Find(&tunnels).Error
	if err != nil {
		h.logger.Error("failed to fetch the tunnel status", logid.StatusTunnelsFetchFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errStatusFetchFailed)
	}

	if tunnels == nil {
		tunnels = []models.Tunnel{}
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data: map[string]interface{}{
			"desired_tunnels":   desiredTunnels,
			"total_tunnels":     totalTunnels,
			"connected_tunnels": connectedTunnels,
			"tunnels":           tunnels,
			"page":              page.number,
			"size":              page.size,
		},
	})
}

func (h *Handler) GetHostStatus(c echo.Context) error {
	hostID, err := strconv.ParseUint(c.Param("hostId"), 10, 32)
	if err != nil {
		return failure(c, http.StatusBadRequest, errHostIDInvalid, errorArgs{"reason": err.Error()})
	}

	var host models.Host
	err = h.db.First(&host, hostID).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return failure(c, http.StatusNotFound, errHostNotFound)
		}
		h.logger.Error("failed to fetch Host", logid.HostFetchFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errHostFetchFailed)
	}

	tunnels, err := h.manager.GetHostTunnels(uint(hostID))
	if err != nil {
		h.logger.Error("failed to fetch the tunnel status of the Host",
			logid.StatusHostTunnelsFetchFailed.Field(), zap.Error(err),
			zap.Uint64("host_id", hostID))
		return failure(c, http.StatusInternalServerError, errStatusFetchFailed)
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
			"host":              hostViewOf(host),
			"total_tunnels":     len(*tunnels),
			"connected_tunnels": connectedTunnels,
			"tunnels":           tunnels,
		},
	})
}
