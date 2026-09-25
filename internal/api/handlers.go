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
	// is reported against is the one the loop works towards. It is what the
	// last reconcile pass wanted, so a write shows in it once the pass the
	// write wakes has run.
	DesiredTunnelCount() (int, error)
	// DesiredLocalForwardCount reports how many local forwards should be
	// running, counted by the manager for the reason DesiredTunnelCount is.
	DesiredLocalForwardCount() (int, error)
	GetAllTunnels() (*[]models.Tunnel, error)
	GetHostTunnels(hostID uint) (*[]models.Tunnel, error)
	// LocalForwardStatuses reports what every running local forward says
	// about itself, keyed by row. It is kept in memory by the manager rather
	// than in a table, so it is asked for here and not read from the rows.
	LocalForwardStatuses() map[tunnel.LocalForwardKey]tunnel.LocalForwardState
	// SocksStatuses reports what every running SOCKS5 proxy says about
	// itself, keyed by Host, for the reason LocalForwardStatuses is asked for.
	SocksStatuses() map[uint]tunnel.SocksState
}

type Handler struct {
	db      *gorm.DB
	manager tunnelManager
	logger  *zap.Logger
	cipher  *crypto.Cipher
	// runningAPIPort is the port this process listens on, which differs from
	// the stored api_port when that one was taken at startup. It is 0 until
	// SetRunningAPIPort is called, and a local port is then held to the stored
	// api_port alone.
	runningAPIPort int
}

func NewHandler(db *gorm.DB, manager tunnelManager, logger *zap.Logger, cipher *crypto.Cipher) *Handler {
	return &Handler{
		db:      db,
		manager: manager,
		logger:  logger,
		cipher:  cipher,
	}
}

// SetRunningAPIPort tells the handler the port this process listens on. It is
// called once, after the listener is opened and before anything is served, so
// the field is not written while a request reads it.
func (h *Handler) SetRunningAPIPort(port int) {
	h.runningAPIPort = port
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

// nextHostID is the number the next Host is registered under.
//
// It is one past the largest in use, which is what the column would hand out if
// it were not AUTOINCREMENT. What that changes is a Host deleted from the end:
// the number it held is free again and the next registration takes it, instead
// of the count climbing away from how many Hosts there are.
//
// A gap in the middle is left alone, though the same query could fill it. The
// number of a Host is what the Status screen, the host key panel and the log
// file call it by, and one handed back in the middle would put a newly
// registered Host among the older ones on every screen that lists them by
// number, under a number an older log line already used for something else.
// Taken from the end, the number that comes back is the one just given up, and
// a new Host is still the last of the list.
//
// It is read inside the transaction that writes the Host. The pool holds one
// connection, so a second registration cannot read the same answer.
func nextHostID(tx *gorm.DB) (uint, error) {
	// Only the number is read, of only the last row. The rest of a Host is the
	// sealed password and the sealed key, and none of it is wanted here.
	//
	// Find and not First: a table with nothing in it is the ordinary case on a
	// fresh installation, and First calls that an error. What it leaves behind
	// is the zero, which is the number before the first.
	var last models.Host

	err := tx.Model(&models.Host{}).Select("id").Order("id desc").Limit(1).Find(&last).Error
	if err != nil {
		return 0, err
	}

	return last.ID + 1, nil
}

// @Summary      Register a Host
// @Description  address is a host name or an IP address. A name is resolved each time the Host is connected to.
// @Description  enabled is optional and a Host that does not say is enabled.
// @Description  bind_scope is what every assignment this registration makes is opened to: loopback, wildcard, or left out for the wildcard. It is read only when the assignments are made.
// @Description  socks_enabled switches on the SOCKS5 proxy of the Host, which needs socks_port. socks_bind_scope is loopback, wildcard, or left out for loopback. socks_allowed_sources is the addresses and CIDR blocks a client may connect from, separated by commas or spaces; empty lets every address in.
// @Tags         hosts
// @Accept   json
// @Produce  json
// @Security  CSRFToken
// @Param   body  body  models.CreateHostRequest  true  "The Host to register"
// @Success  200  {object}  models.Response{data=api.hostView}
// @Failure  400  {object}  api.errorBody  "The body is refused, the private key cannot be read, or the SOCKS5 proxy is switched on without a port or with allowed sources that do not read"
// @Failure  409  {object}  api.errorBody  "socks_port is the port of this server, of the SOCKS5 proxy of another Host or of a local forward"
// @Router       /host [post]
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

	// A scope left out is this machine alone. The proxy asks for no password,
	// so opening it to other machines is a choice made by naming the wildcard,
	// not one a request makes by saying nothing.
	socksBindScope := req.SocksBindScope
	if socksBindScope == "" {
		socksBindScope = models.BindScopeLoopback
	}

	socks := models.Host{
		SocksEnabled:        req.SocksEnabled,
		SocksPort:           req.SocksPort,
		SocksBindScope:      socksBindScope,
		SocksAllowedSources: req.SocksAllowedSources,
	}

	refused := checkSocks(&socks)
	if refused != nil {
		return refused.answer(c)
	}

	// Read before the transaction for the reason storedAPIPort gives, and only
	// for a Host whose proxy is switched on, since nothing else is held to it.
	apiPort := 0

	if socks.SocksEnabled {
		apiPort, err = h.storedAPIPort()
		if err != nil {
			h.logger.Error("failed to read the settings", logid.SettingsReadFailed.Field(), zap.Error(err))
			return failure(c, http.StatusInternalServerError, errSettingsReadFailed)
		}
	}

	tx := h.db.Begin()
	err = tx.Error
	if err != nil {
		h.logger.Error("failed to start the transaction", logid.DatabaseTransactionStartFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionBeginFailed)
	}
	defer rollbackUnlessDone(tx)

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
		Address:       req.Address,
		Port:          req.Port,
		User:          req.User,
		Password:      password,
		PrivateKey:    privateKey,
		KeyPassphrase: keyPassphrase,
		Description:   req.Description,
		Enabled:       enabled,

		SocksEnabled:        socks.SocksEnabled,
		SocksPort:           socks.SocksPort,
		SocksBindScope:      socks.SocksBindScope,
		SocksAllowedSources: socks.SocksAllowedSources,
	}

	// The number is chosen here rather than left to the column, so that one a
	// deleted Host gave up is handed out again. nextHostID says which one.
	host.ID, err = nextHostID(tx)
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to work out the number for a new Host",
			logid.HostNextNumberReadFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errHostCreateFailed)
	}

	if host.SocksEnabled {
		refused, err = socksPortRefused(tx, host.ID, host.SocksPort, apiPort, h.runningAPIPort)
		if err != nil {
			tx.Rollback()
			h.logger.Error("failed to fetch Hosts", logid.HostListFetchFailed.Field(),
				zap.Error(err), zap.Int("socks_port", host.SocksPort))
			return failure(c, http.StatusInternalServerError, errHostCreateFailed)
		}
		if refused != nil {
			tx.Rollback()
			return refused.answer(c)
		}
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

		// Every assignment of the batch is opened to what the request asked
		// for. The Host holds no scope of its own, so an answer given while it
		// is being registered has nowhere to be kept but on the rows it makes,
		// and a batch written without it would be a Host on the wildcard that
		// nobody asked to put there.
		for _, spID := range spIDs {
			err = tx.Create(&models.HostServicePort{HostID: host.ID, SPID: spID, BindScope: req.BindScope}).Error
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
		Data:    hostViewOf(*host, h.manager.SocksStatuses()),
	})
}

// ListHosts answers one page of the Hosts, in the order they were registered
// in. The order is stated rather than left to the database, because LIMIT and
// OFFSET cut a page out of an order: rows handed back in a different order from
// one read to the next would put one Host on two pages and another on none. The
// id is what that order is taken from, since it is given out once, never
// changes, and no two Hosts share one.
//
// @Summary      One page of the Hosts, oldest first
// @Tags         hosts
// @Produce  json
// @Param   page  query  int  false  "The page, counted from 1. Below 1 is read as 1, and a page past the last one is answered with the last page"
// @Param   size  query  int  false  "How many rows a page holds"  Enums(10, 20, 30, 50, 100)
// @Param   q     query  string  false  "Only the Hosts whose address, user, description or SSH port holds this text, with ASCII letters matched in either case. % and _ are taken as written"
// @Success  200  {object}  models.Response{data=api.listPageOf{items=[]api.hostView}}
// @Failure  400  {object}  api.errorBody  "page is not a number, or size is not one of the sizes taken"
// @Router       /host [get]
func (h *Handler) ListHosts(c echo.Context) error {
	page, refused := readListPage(c)
	if refused != nil {
		return badListPage(c, refused)
	}

	// The rows are counted by the database rather than read and measured here.
	// Reading every row to find out how many there are is the work the paging
	// is here to avoid.
	q := readListSearch(c)

	var total int64
	err := h.db.Model(&models.Host{}).Scopes(hostsMatching(q)).Count(&total).Error
	if err != nil {
		h.logger.Error("failed to count the Hosts", logid.HostCountFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errHostListFailed)
	}

	page = page.fitTo(total)

	var hosts []models.Host
	err = h.db.Scopes(hostsMatching(q)).Order("id").Limit(page.size).Offset(page.offset()).Find(&hosts).Error
	if err != nil {
		h.logger.Error("failed to fetch Hosts", logid.HostListFetchFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errHostListFailed)
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data: listPageOf{
			Items: hostViewsOf(hosts, h.manager.SocksStatuses()),
			Total: total,
			Page:  page.number,
			Size:  page.size,
		},
	})
}

// @Summary      Read one Host
// @Tags         hosts
// @Produce  json
// @Param   id  path  int  true  "The id of the Host"
// @Success  200  {object}  models.Response{data=api.hostView}
// @Failure  404  {object}  api.errorBody  "No such Host"
// @Router       /host/{id} [get]
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
		Data:    hostViewOf(host, h.manager.SocksStatuses()),
	})
}

// @Summary      Update a Host
// @Description  Every field is optional; enabled false stops its tunnels.
// @Description  A SOCKS5 field left out keeps what is stored. socks_allowed_sources sent empty lets every address in; socks_bind_scope left empty keeps the stored scope.
// @Tags         hosts
// @Accept   json
// @Produce  json
// @Security  CSRFToken
// @Param   id    path  int  true  "The id of the Host"
// @Param   body  body  models.UpdateHostRequest  true  "The fields to change"
// @Success  200  {object}  models.Response{data=api.hostView}
// @Failure  400  {object}  api.errorBody  "The body is refused"
// @Failure  404  {object}  api.errorBody  "No such Host"
// @Failure  409  {object}  api.errorBody  "The SOCKS5 proxy is switched on or moved to the port of this server, of the SOCKS5 proxy of another Host or of a local forward"
// @Router       /host/{id} [put]
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

	// Read before the transaction for the reason storedAPIPort gives, and only
	// for a request that may open a SOCKS5 proxy on another port.
	apiPort := 0

	if req.SocksEnabled != nil || req.SocksPort != nil {
		apiPort, err = h.storedAPIPort()
		if err != nil {
			h.logger.Error("failed to read the settings", logid.SettingsReadFailed.Field(), zap.Error(err))
			return failure(c, http.StatusInternalServerError, errSettingsReadFailed)
		}
	}

	tx := h.db.Begin()
	err = tx.Error
	if err != nil {
		h.logger.Error("failed to start the transaction", logid.DatabaseTransactionStartFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionBeginFailed)
	}
	defer rollbackUnlessDone(tx)

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

	if req.Address != "" {
		host.Address = req.Address
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

	socksBefore := host.SocksEnabled && host.SocksPort > 0
	portBefore := host.SocksPort

	if req.SocksEnabled != nil {
		host.SocksEnabled = *req.SocksEnabled
	}
	if req.SocksPort != nil {
		host.SocksPort = *req.SocksPort
	}
	if req.SocksBindScope != "" {
		host.SocksBindScope = req.SocksBindScope
	}
	if req.SocksAllowedSources != nil {
		host.SocksAllowedSources = *req.SocksAllowedSources
	}

	refused := checkSocks(&host)
	if refused != nil {
		tx.Rollback()
		return refused.answer(c)
	}

	// The port is held to the rest of the machine only when this change is
	// what opens it, so that a change of another field is not refused over a
	// port that is stored already.
	if host.SocksEnabled && (!socksBefore || host.SocksPort != portBefore) {
		refused, err = socksPortRefused(tx, host.ID, host.SocksPort, apiPort, h.runningAPIPort)
		if err != nil {
			tx.Rollback()
			h.logger.Error("failed to fetch Hosts", logid.HostListFetchFailed.Field(),
				zap.Error(err), zap.Int("socks_port", host.SocksPort))
			return failure(c, http.StatusInternalServerError, errHostUpdateFailed)
		}
		if refused != nil {
			tx.Rollback()
			return refused.answer(c)
		}
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
		Data:    hostViewOf(host, h.manager.SocksStatuses()),
	})
}

// @Summary      Delete a Host, and the assignments naming it
// @Tags         hosts
// @Produce  json
// @Security  CSRFToken
// @Param   id  path  int  true  "The id of the Host"
// @Success  200  {object}  models.Response{data=string}
// @Failure  404  {object}  api.errorBody  "No such Host"
// @Router       /host/{id} [delete]
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
	defer rollbackUnlessDone(tx)

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

	// The local forwards of this Host go with it for the same reason. The
	// line carries HostDeleteFailed rather than an identifier of its own, since
	// the Host is what failed to be deleted and the rollback leaves it stored.
	err = tx.Where("host_id = ?", host.ID).Delete(&models.LocalForward{}).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to delete Host", logid.HostDeleteFailed.Field(), zap.Error(err), zap.Uint64("host_id", id))
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

// @Summary      Register a service port
// @Description  assign_to_all_hosts is optional: left out, every stored Host is given it, and sent as false it is registered carried by none.
// @Description  bind_scope is what the assignments it makes are opened to: loopback, wildcard, or left out for the wildcard.
// @Tags         service ports
// @Accept   json
// @Produce  json
// @Security  CSRFToken
// @Param   body  body  models.CreateServicePortRequest  true  "The service port to register"
// @Success  200  {object}  models.Response{data=models.ServicePort}
// @Failure  400  {object}  api.errorBody  "The body is refused, or the local port is already taken"
// @Router       /service-port [post]
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
		ServiceAddress: req.ServiceAddress,
		ServicePort:    req.ServicePort,
		LocalPort:      req.LocalPort,
		Description:    req.Description,
	}

	tx := h.db.Begin()
	err = tx.Error
	if err != nil {
		h.logger.Error("failed to start the transaction", logid.DatabaseTransactionStartFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionBeginFailed)
	}
	defer rollbackUnlessDone(tx)

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

		// The batch is opened to what the request asked for, as the batch a
		// registration makes is. The two words name the same pair of addresses
		// on every Host, which is what lets one answer stand for assignments
		// spread over all of them.
		for _, hostID := range hostIDs {
			err = tx.Create(&models.HostServicePort{HostID: hostID, SPID: sp.ID, BindScope: req.BindScope}).Error
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
//
// @Summary      One page of the service ports, oldest first
// @Tags         service ports
// @Produce  json
// @Param   page  query  int  false  "The page, counted from 1. Below 1 is read as 1, and a page past the last one is answered with the last page"
// @Param   size  query  int  false  "How many rows a page holds"  Enums(10, 20, 30, 50, 100)
// @Param   q     query  string  false  "Only the service ports whose service address, service port, local port or description holds this text, with ASCII letters matched in either case. % and _ are taken as written"
// @Success  200  {object}  models.Response{data=api.listPageOf{items=[]models.ServicePort}}
// @Failure  400  {object}  api.errorBody  "page is not a number, or size is not one of the sizes taken"
// @Router       /service-port [get]
func (h *Handler) ListServicePorts(c echo.Context) error {
	page, refused := readListPage(c)
	if refused != nil {
		return badListPage(c, refused)
	}

	q := readListSearch(c)

	var total int64
	err := h.db.Model(&models.ServicePort{}).Scopes(servicePortsMatching(q)).Count(&total).Error
	if err != nil {
		h.logger.Error("failed to count the service ports", logid.ServicePortCountFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errServicePortListFailed)
	}

	page = page.fitTo(total)

	var sps []models.ServicePort
	err = h.db.Scopes(servicePortsMatching(q)).Order("id").Limit(page.size).Offset(page.offset()).Find(&sps).Error
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

// @Summary      Read one service port
// @Tags         service ports
// @Produce  json
// @Param   id  path  int  true  "The id of the service port"
// @Success  200  {object}  models.Response{data=models.ServicePort}
// @Failure  404  {object}  api.errorBody  "No such service port"
// @Router       /service-port/{id} [get]
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

// @Summary      Update a service port
// @Description  service_address, service_port and local_port are all required. service_address is a host name or an IP address, resolved on this system each time a connection is forwarded.
// @Tags         service ports
// @Accept   json
// @Produce  json
// @Security  CSRFToken
// @Param   id    path  int  true  "The id of the service port"
// @Param   body  body  models.CreateServicePortRequest  true  "The service port as it should stand"
// @Success  200  {object}  models.Response{data=models.ServicePort}
// @Failure  400  {object}  api.errorBody  "The body is refused, or the local port is already taken"
// @Failure  404  {object}  api.errorBody  "No such service port"
// @Router       /service-port/{id} [put]
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
	defer rollbackUnlessDone(tx)

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

	sp.ServiceAddress = req.ServiceAddress
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

// @Summary      Delete a service port, and the assignments naming it
// @Tags         service ports
// @Produce  json
// @Security  CSRFToken
// @Param   id  path  int  true  "The id of the service port"
// @Success  200  {object}  models.Response{data=string}
// @Failure  404  {object}  api.errorBody  "No such service port"
// @Router       /service-port/{id} [delete]
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
	defer rollbackUnlessDone(tx)

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
	// BindScope is what the assignment of this Host is opened to, and is the
	// empty value on a row the Host does not carry: the scope is held by the
	// assignment, so a service port that is not assigned has none. The empty
	// value is the wildcard wherever it is stored, which is what the row this
	// screen would make carries until something else is chosen.
	BindScope string `json:"bind_scope"`
}

// ListHostServicePorts answers one page of the service ports with the
// assignments of one Host laid over them.
//
// The page is taken over the service ports and not over the assignments, and
// ordered by id as ListServicePorts is, so that a row sits on the same page of
// both lists whether this Host carries it or not.
//
// @Summary      One page of the service ports, with assigned saying whether this Host carries each
// @Description  The page is taken over the service ports and not over the assignments, so a row sits on the same page of this list and of GET /api/service-port whether the Host carries it or not.
// @Description  bind_scope is what the assignment of this Host is opened to, and is empty on a row the Host does not carry.
// @Tags         assignments
// @Produce  json
// @Param   page  query  int  false  "The page, counted from 1. Below 1 is read as 1, and a page past the last one is answered with the last page"
// @Param   size  query  int  false  "How many rows a page holds"  Enums(10, 20, 30, 50, 100)
// @Param   id  path  int  true  "The id of the Host"
// @Success  200  {object}  models.Response{data=api.listPageOf{items=[]api.hostServicePortItem}}
// @Failure  400  {object}  api.errorBody  "page is not a number, or size is not one of the sizes taken"
// @Failure  404  {object}  api.errorBody  "No such Host"
// @Router       /host/{id}/service-port [get]
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

	// The whole row is read and not the identifier alone, because what an
	// assignment is opened to is on it and the screen draws that beside the
	// box. Whether the Host carries the service port is then whether the row
	// is there, which the scope cannot answer: the empty value is a scope a
	// stored assignment may hold.
	assigned := make(map[uint]models.HostServicePort, len(ids))
	if len(ids) > 0 {
		var rows []models.HostServicePort
		err = h.db.Where("host_id = ? AND sp_id IN ?", host.ID, ids).Find(&rows).Error
		if err != nil {
			h.logger.Error("failed to fetch the service port assignments of a Host",
				logid.HostServicePortAssignmentsFetchFailed.Field(),
				zap.Error(err), zap.Uint64("host_id", id))
			return failure(c, http.StatusInternalServerError, errServicePortListFailed)
		}

		for _, row := range rows {
			assigned[row.SPID] = row
		}
	}

	// An empty page is an array and not null, for the reason ListHosts says.
	items := make([]hostServicePortItem, 0, len(sps))
	for _, sp := range sps {
		row, carried := assigned[sp.ID]
		items = append(items, hostServicePortItem{
			ServicePort: sp,
			Assigned:    carried,
			BindScope:   row.BindScope,
		})
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
	// BindScope is what the assignments this change writes are opened to, and
	// what the ones named in Rescope are moved to. An empty value is the
	// wildcard, the way it is on the column itself, so a client written before
	// the field existed writes what it always wrote.
	BindScope string `json:"bind_scope" validate:"omitempty,oneof=loopback wildcard"`
	// Rescope names the assignments to move to BindScope, which is how one is
	// changed without being taken away and put back: an identifier on its own
	// is the row edited by itself, and several of them are the scope applied
	// to everything that was ticked.
	//
	// It is a list of its own rather than Add doing the moving, because Add
	// leaves an assignment that is already there exactly as it is. Read the
	// other way, a client that ticks a box which was ticked already, or one
	// written before any of this existed, would widen to the wildcard an
	// assignment somebody had pinned to loopback, and a reach given away by a
	// request that did not mention it is the one thing this must not do.
	//
	// An identifier here that the Host does not carry moves nothing, the way
	// Remove takes no row that is not there. What the answer reports is rows
	// and not identifiers.
	Rescope []uint `json:"rescope"`
}

// hostServicePortChanged is what the change did: how many assignments it wrote
// and how many it removed. The two are counted over the rows and not over the
// request, because a service port already assigned is asked for again without
// being written, and one that is not assigned is removed without a row going.
type hostServicePortChanged struct {
	Added   int `json:"added"`
	Removed int `json:"removed"`
	// Rescoped is how many assignments were moved to the scope the change
	// named, counted over the rows for the same reason: an identifier naming a
	// service port this Host does not carry moves nothing.
	Rescoped int `json:"rescoped"`
}

// UpdateHostServicePorts adds and removes assignments of one Host, and moves
// the ones it is told to move to the bind scope the change carries.
//
// The whole of the change lands or none of it does. The reconcile loop reads
// these rows to decide which tunnels to run, so a change that landed in part
// would leave the installation carrying traffic over a set of tunnels that no
// request asked for, and nothing later would put it right.
//
// @Summary      Add and remove assignments of this Host
// @Description  Takes a change and not the whole set: both lists are optional, and a request that changes nothing is answered rather than refused.
// @Description  added, removed and rescoped count the rows written and not the ids sent.
// @Description  bind_scope is what the added assignments are opened to and what the ones named in rescope are moved to: loopback, wildcard, or left out for the wildcard. An assignment that is already there is left on the scope it holds unless rescope names it.
// @Tags         assignments
// @Accept   json
// @Produce  json
// @Security  CSRFToken
// @Param   id    path  int  true  "The id of the Host"
// @Param   body  body  api.hostServicePortChange  true  "The service port ids to add, to remove and to move to bind_scope"
// @Success  200  {object}  models.Response{data=api.hostServicePortChanged}
// @Failure  400  {object}  api.errorBody  "The same service port is in both lists, an id is not stored, or bind_scope is neither loopback nor wildcard"
// @Failure  404  {object}  api.errorBody  "No such Host"
// @Router       /host/{id}/service-port [put]
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

	// The scope is held to the two words it may be. The column is under the
	// same rule in the database, which would refuse a third one with a failure
	// that says nothing to whoever sent it, and this refusal names the field.
	err = c.Validate(&req)
	if err != nil {
		return failure(c, http.StatusBadRequest, errRequestValidationFailed, errorArgs{"reason": err.Error()})
	}

	add := sortedIDs(req.Add)
	remove := sortedIDs(req.Remove)
	rescope := sortedIDs(req.Rescope)

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
	defer rollbackUnlessDone(tx)

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
			err = tx.Create(&models.HostServicePort{HostID: host.ID, SPID: spID, BindScope: req.BindScope}).Error
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

	// The assignments named are moved to the scope the change carries. It runs
	// after the adds, so that a request adding an assignment and naming it here
	// as well leaves it on that scope either way, and before the removes, so
	// that an identifier on both sides ends up gone: taking an assignment away
	// is the larger of the two statements, and the row moved first is the row
	// removed second.
	//
	// The update is written straight to the column rather than read and saved
	// back. The rows are the ones the change named, the value is the one it
	// carries, and what is counted is how many rows the database wrote.
	rescoped := 0
	if len(rescope) > 0 {
		result := tx.Model(&models.HostServicePort{}).
			Where("host_id = ? AND sp_id IN ?", host.ID, rescope).
			Update("bind_scope", req.BindScope)
		if result.Error != nil {
			tx.Rollback()
			h.logger.Error("failed to move the service port assignments of a Host to a bind scope",
				logid.HostServicePortAssignFailed.Field(),
				zap.Error(result.Error), zap.Uint64("host_id", id))
			return failure(c, http.StatusInternalServerError, errAssignmentUpdateFailed)
		}

		rescoped = int(result.RowsAffected)
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
	// A scope that was moved wakes it as well. How far a forwarded port reaches
	// is what the loop asks the far side for, so a row that changed scope is a
	// tunnel that has to be made again.
	if added > 0 || removed > 0 || rescoped > 0 {
		h.manager.WakeReconcile()
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    hostServicePortChanged{Added: added, Removed: removed, Rescoped: rescoped},
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
// rows of the status table: the service port tunnels and the local forwards
// together, each row saying which of the two it is on kind.
//
// The counts are over every row and not over the page. They are what the
// status screen says the installation is doing, and counted over a page they
// would follow the page size around: total_rows would read as the size of the
// page, and a page without a connected tunnel on it would say that nothing is
// connected while the tunnels carry traffic.
//
// There are four of them and each counts both sorts of forward together, a
// local forward being a tunnel to whoever reads this answer. desired_tunnels
// is what should be running, as the last reconcile pass saw it: the manager
// keeps the number from the pass rather than reading the tables for it on
// every refresh of the screen, so a write shows in it once the pass it wakes
// has run. connected_tunnels, reconnecting_tunnels and error_tunnels are how
// many rows carry each of those statuses. The last two are apart because what
// they leave an operator to do differs: a row that is reconnecting is on its
// way back on its own, and one in error is waiting for somebody, and a single
// number over the two says nothing about which.
//
// The four do not add up to desired_tunnels, and are not meant to. A row that
// is still starting or that is held up at a host key is in none of the three,
// and a row that runs while nothing wants it is in one of them and not in
// desired_tunnels.
//
// What paging is done against is none of the four: it is total_rows, the rows
// of both tables, since the page is a cut through the two of them.
//
// q narrows the rows to the ones whose Host has it in its address or its
// description, or whose local or remote address has it. It narrows total_rows
// and the pages with them, and not the four counts: those say what the
// installation is doing, and a search on the screen does not change that.
//
// The page is ordered by Host, then by sort, then by the id the row carries
// within its sort. Host first is what keeps the rows of one Host together: an
// order that took the tunnels first and the forwards after would put the two
// halves of a Host pages apart. The order is stated rather than left to the
// database for the reason ListHosts states one; see there for what an order
// that is not stated does to LIMIT and OFFSET.
//
// @Summary      The counts of the installation and one page of the status rows
// @Description  tunnels carries both sorts of forward: kind is service_port or local_forward, and sp_id is null on a local forward, which is carried by no service port. On a local forward row, local is the address opened on this machine and remote the target reached from the Host, which is the mirror of what they hold on a service port row. forward_reach is on both sorts: on a service port row it says whether the port opened on the Host answered a connection from here, and on a local forward row whether the target answered one dialled from the Host.
// @Description  The counts are over every row and not over the page: they say what the installation is doing, not what is on the page being looked at. There are four, and each counts the service port tunnels and the local forwards together: desired_tunnels is what should be running as of the last reconcile pass, which a change wakes, and connected_tunnels, reconnecting_tunnels and error_tunnels are how many rows are in each of those statuses. A row that is starting or held up at a host key is in none of the three. total_rows is the rows of both sorts, which is what the pages are cut from.
// @Description  q narrows the rows and total_rows with them, and leaves the four counts over every row.
// @Tags         status
// @Produce  json
// @Param   page  query  int  false  "The page, counted from 1. Below 1 is read as 1, and a page past the last one is answered with the last page"
// @Param   size  query  int  false  "How many rows a page holds"  Enums(10, 20, 30, 50, 100)
// @Param   q     query  string  false  "Only the rows whose Host holds this text in its address or description, or whose local or remote address holds it, with ASCII letters matched in either case. % and _ are taken as written"
// @Success  200  {object}  models.Response  "counts, and tunnels holding one page of the status rows"
// @Failure  400  {object}  api.errorBody  "page is not a number, or size is not one of the sizes taken"
// @Router       /status [get]
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

	// The same count over the local forwards. It is asked of the manager for
	// the reason the tunnels are: what should be running is decided by the
	// pass, and a rule written out again here would be a second place keeping
	// it and would drift from the loop the moment either changed.
	//
	// A failed read is answered under the refusal of the tunnel count. The two
	// are one failure to the reader of this answer, and a code of its own
	// would be a message to write in every language the screen is served in.
	desiredLocalForwards, err := h.manager.DesiredLocalForwardCount()
	if err != nil {
		h.logger.Error("failed to count the local forwards that should be running",
			logid.LocalForwardCountFailed.Field(),
			zap.Error(err))
		return failure(c, http.StatusInternalServerError, errStatusDesiredCountFailed)
	}

	tunnelCounts, err := tunnelStatusCounts(h.db)
	if err != nil {
		h.logger.Error("failed to count the tunnels by status",
			logid.StatusConnectedTunnelsCountFailed.Field(),
			zap.Error(err))
		return failure(c, http.StatusInternalServerError, errStatusFetchFailed)
	}

	// The Hosts waiting for a host key approval are counted here and not listed
	// here, and the two counts are over every Host rather than over the page of
	// tunnels below.
	//
	// Over the page they would say nothing: a Host with four service ports has
	// four tunnels refused by the same key, so a page would carry the same
	// question four times and a Host whose tunnels are all on the next page
	// would not be asked about at all. What goes on the screen from these is
	// one line over the table, and the list behind it is read a page at a time
	// from ListHostKeysWaiting: an upgrade leaves every Host waiting at once,
	// and a list of two hundred of them in the answer this screen refreshes
	// itself from every few seconds is the load the paging is here to avoid.
	var hostKeysUnapproved int64

	err = hostKeysFirstApproval(h.db).Count(&hostKeysUnapproved).Error
	if err != nil {
		h.logger.Error("failed to count the Hosts", logid.HostCountFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errStatusFetchFailed)
	}

	var hostKeysMismatched int64

	err = hostKeysChanged(h.db).Count(&hostKeysMismatched).Error
	if err != nil {
		h.logger.Error("failed to count the Hosts", logid.HostCountFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errStatusFetchFailed)
	}

	// Where the page falls is worked out on the keys of both tables rather
	// than on the rows, because the page is a cut through the two of them at
	// once and neither table can say on its own where that cut is. The two
	// reads carry two columns each.
	//
	// The number of rows is counted off these lists rather than by a COUNT of
	// its own, so that what the answer says there and the page it hands over
	// are the one read. Two reads can straddle a row being written, and then
	// the screen is told a number of rows the page it was given does not fit.
	//
	// A search narrows both lists before the page is cut, so that the page and
	// total_rows are taken over the rows that match. The tunnels are narrowed
	// in SQL, the same way on this read and on the read of the page. The local
	// forwards that match are read whole, and the page is cut out of them
	// below; localForwardsMatching says why.
	q := readListSearch(c)

	tunnelRefs, err := statusTunnelRefs(h.db.Scopes(tunnelsMatching(q)))
	if err != nil {
		h.logger.Error("failed to count the tunnel rows", logid.StatusTunnelRowsCountFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errStatusFetchFailed)
	}

	var (
		forwardRefs     []statusRef
		matchedForwards []models.LocalForward
	)

	if q == "" {
		forwardRefs, err = statusLocalForwardRefs(h.db)
	} else {
		matchedForwards, err = localForwardsMatching(h.db, q)
		forwardRefs = localForwardRefsOf(matchedForwards)
	}

	if err != nil {
		h.logger.Error("failed to count the local forwards", logid.LocalForwardCountFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errStatusFetchFailed)
	}

	totalTunnels := int64(len(tunnelRefs))
	totalLocalForwards := int64(len(forwardRefs))

	refs := mergeStatusRefs(tunnelRefs, forwardRefs)

	page = page.fitTo(int64(len(refs)))
	split := splitStatusPage(refs, page.offset(), page.size)

	// Each table is then read with the LIMIT and OFFSET its own run on the
	// page comes to. A table with no row on this page is not read at all.
	var tunnels []models.Tunnel
	if split.tunnelCount > 0 {
		err = h.db.Scopes(tunnelsMatching(q)).Order("host_id, sp_id").
			Limit(split.tunnelCount).Offset(split.tunnelOffset).Find(&tunnels).Error
		if err != nil {
			h.logger.Error("failed to fetch the tunnel status", logid.StatusTunnelsFetchFailed.Field(), zap.Error(err))
			return failure(c, http.StatusInternalServerError, errStatusFetchFailed)
		}
	}

	var forwards []models.LocalForward
	if split.forwardCount > 0 && q != "" {
		forwards = matchedForwards[split.forwardOffset : split.forwardOffset+split.forwardCount]
	} else if split.forwardCount > 0 {
		err = h.db.Order("host_id, number").
			Limit(split.forwardCount).Offset(split.forwardOffset).Find(&forwards).Error
		if err != nil {
			h.logger.Error("failed to fetch local forwards", logid.LocalForwardListFetchFailed.Field(), zap.Error(err))
			return failure(c, http.StatusInternalServerError, errStatusFetchFailed)
		}
	}

	// The Hosts of the forwards on this page, which is what says whether a
	// forward reports at all and where its SSH connection goes. Only the Hosts
	// of the page are read: what is on the other pages is not being answered.
	hostByID, err := h.hostsOfLocalForwards(forwards)
	if err != nil {
		h.logger.Error("failed to fetch Host", logid.HostFetchFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errStatusFetchFailed)
	}

	states := h.manager.LocalForwardStatuses()

	tunnelRows := make([]statusPageRow, 0, len(tunnels))
	for _, t := range tunnels {
		tunnelRows = append(tunnelRows, tunnelStatusRow(t))
	}

	forwardRows := make([]statusPageRow, 0, len(forwards))
	for _, lf := range forwards {
		forwardRows = append(forwardRows, localForwardStatusRow(lf, hostByID[lf.HostID], states))
	}

	rows := mergeStatusRows(tunnelRows, forwardRows)

	// The counts of the two sorts are added together here, at the one place
	// they leave for the screen. Each sort is counted where its status lives -
	// the tunnels in a column, the local forwards in what the manager holds -
	// and neither can be counted the way the other is.
	counts := tunnelCounts.plus(localForwardStatusCounts(states))

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data: map[string]interface{}{
			"desired_tunnels":      desiredTunnels + desiredLocalForwards,
			"connected_tunnels":    counts.connected,
			"reconnecting_tunnels": counts.reconnecting,
			"error_tunnels":        counts.errored,
			"total_rows":           totalTunnels + totalLocalForwards,
			"host_keys_unapproved": hostKeysUnapproved,
			"host_keys_mismatched": hostKeysMismatched,
			"tunnels":              rows,
			"page":                 page.number,
			"size":                 page.size,
		},
	})
}

// hostsOfLocalForwards reads the Hosts the given forwards are carried by,
// keyed by id. A Host that is not there is simply not in the map, which is how
// a row left over from before a Host was deleted is answered.
func (h *Handler) hostsOfLocalForwards(forwards []models.LocalForward) (map[uint]*models.Host, error) {
	if len(forwards) == 0 {
		return nil, nil
	}

	ids := make([]uint, 0, len(forwards))
	seen := make(map[uint]bool, len(forwards))

	for _, lf := range forwards {
		if seen[lf.HostID] {
			continue
		}

		seen[lf.HostID] = true
		ids = append(ids, lf.HostID)
	}

	var hosts []models.Host
	err := h.db.Where("id IN ?", ids).Find(&hosts).Error
	if err != nil {
		return nil, err
	}

	hostByID := make(map[uint]*models.Host, len(hosts))
	for i := range hosts {
		hostByID[hosts[i].ID] = &hosts[i]
	}

	return hostByID, nil
}

// @Summary      The Host and the tunnels of that Host
// @Description  Not paged: a Host holds one tunnel per service port it carries.
// @Tags         status
// @Produce  json
// @Param   hostId  path  int  true  "The id of the Host"
// @Success  200  {object}  models.Response  "host, total_tunnels, connected_tunnels and tunnels"
// @Failure  404  {object}  api.errorBody  "No such Host"
// @Router       /status/{hostId} [get]
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
			"host":              hostViewOf(host, h.manager.SocksStatuses()),
			"total_tunnels":     len(*tunnels),
			"connected_tunnels": connectedTunnels,
			"tunnels":           tunnels,
		},
	})
}
