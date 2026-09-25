package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// An API token is what a script sends in place of the session cookie, in an
// Authorization: Bearer header. It is made on the Settings screen by someone
// who is signed in, it is shown once, and what it opens is decided per route
// by the scopes it was made with.
//
// The CSRF check is not asked of a request that brings one. That check exists
// because a browser attaches the session cookie to a request it was tricked
// into sending; it never attaches an Authorization header of its own accord,
// so a request that carries a token was written by something that holds it.

// apiTokenPrefix is what every token begins with. It is there so that a token
// pasted somewhere it should not be is recognisable for what it is, by a person
// and by a secret scanner.
const apiTokenPrefix = "tm_"

// apiTokenBytes is how much randomness a token carries. It is the size of a
// session token, for the same reason: the token is the whole credential.
const apiTokenBytes = 32

// bearerScheme is the scheme of the Authorization header a token arrives in.
const bearerScheme = "Bearer"

// tokenNameMaxLength is the longest name a token takes, counted in characters.
// The name is written into the log beside every change made with the token, so
// it is held to the length of a label rather than a paragraph.
const tokenNameMaxLength = 64

// tokenLastUsedResolution is how far last_used_at may lag behind. The column
// is written at most once a minute per token, rather than on every request, so
// that a script polling the status every few seconds does not turn each of its
// reads into a write on the one connection the database has.
const tokenLastUsedResolution = time.Minute

// tokenDefaultExpiryDays is how long a token lives when the request does not
// say. It is the default the Settings screen offers as well.
const tokenDefaultExpiryDays = 90

// tokenExpiryDays are the lifetimes a token can be made with, in days. Zero is
// a token that never runs out.
var tokenExpiryDays = []int{30, 90, 365, 0}

// The scopes a token can be made with. Each one opens a set of routes, and the
// table below says which. They are coarse on purpose: a scope per route would
// be a list of checkboxes nobody reads before ticking all of them.
const (
	// TokenScopeRead opens every read. It is what a monitoring script needs,
	// and it is the one scope ticked by default.
	TokenScopeRead = "read"
	// TokenScopeHosts opens adding, changing and deleting a Host.
	TokenScopeHosts = "hosts"
	// TokenScopeTunnels opens the service ports, which of them a Host
	// carries, and the local forwards of a Host.
	TokenScopeTunnels = "tunnels"
	// TokenScopeHostKeys opens the approval of a host key.
	TokenScopeHostKeys = "host-keys"
	// TokenScopeSettings opens the settings and the certificate.
	TokenScopeSettings = "settings"
	// TokenScopeTransfer opens the exports and the imports.
	TokenScopeTransfer = "transfer"
	// TokenScopeOperations opens what acts on the process itself: the
	// restart, the update, the uninstall and emptying the log.
	TokenScopeOperations = "operations"
)

// tokenScopes is every scope, in the order the screen lists them and the order
// a token's scopes are stored in.
var tokenScopes = []string{
	TokenScopeRead,
	TokenScopeHosts,
	TokenScopeTunnels,
	TokenScopeHostKeys,
	TokenScopeSettings,
	TokenScopeTransfer,
	TokenScopeOperations,
}

// What TokenRouteRule answers for a route that no scope opens.
const (
	// TokenRouteNever is a route a token never reaches, whatever it was made
	// with.
	TokenRouteNever = "never"
	// TokenRoutePublic is a route that is answered before any credential is
	// looked at, so a token has nothing to do with it.
	TokenRoutePublic = "public"
)

// tokenRouteScopes is the scope each route needs, keyed by the method and the
// route as echo names it. A route that is in none of the three tables is
// refused to a token, so a route added later is closed to tokens until it is
// written in here. TestEveryRouteIsInTheTokenTable in package main is what
// makes that show.
var tokenRouteScopes = map[string]string{
	"GET /api/account":                        TokenScopeRead,
	"GET /api/host":                           TokenScopeRead,
	"GET /api/host/:id":                       TokenScopeRead,
	"GET /api/host-key":                       TokenScopeRead,
	"GET /api/host/:id/service-port":          TokenScopeRead,
	"GET /api/host/:id/local-forward":         TokenScopeRead,
	"GET /api/host/:id/local-forward/:number": TokenScopeRead,
	"GET /api/service-port":                   TokenScopeRead,
	"GET /api/service-port/:id":               TokenScopeRead,
	"GET /api/status":                         TokenScopeRead,
	"GET /api/status/:hostId":                 TokenScopeRead,
	"GET /api/settings":                       TokenScopeRead,
	"GET /api/certificate":                    TokenScopeRead,
	"GET /api/logs":                           TokenScopeRead,
	"GET /api/restart":                        TokenScopeRead,
	"GET /api/update":                         TokenScopeRead,

	"POST /api/host":       TokenScopeHosts,
	"PUT /api/host/:id":    TokenScopeHosts,
	"DELETE /api/host/:id": TokenScopeHosts,

	"POST /api/service-port":                     TokenScopeTunnels,
	"PUT /api/service-port/:id":                  TokenScopeTunnels,
	"DELETE /api/service-port/:id":               TokenScopeTunnels,
	"PUT /api/host/:id/service-port":             TokenScopeTunnels,
	"POST /api/host/:id/local-forward":           TokenScopeTunnels,
	"PUT /api/host/:id/local-forward/:number":    TokenScopeTunnels,
	"DELETE /api/host/:id/local-forward/:number": TokenScopeTunnels,

	"POST /api/host/:id/host-key": TokenScopeHostKeys,
	"POST /api/host-key":          TokenScopeHostKeys,

	"PUT /api/settings":                     TokenScopeSettings,
	"POST /api/settings/alert/test-webhook": TokenScopeSettings,
	"POST /api/settings/alert/test-smtp":    TokenScopeSettings,
	"POST /api/certificate/renew":           TokenScopeSettings,
	"PUT /api/certificate":                  TokenScopeSettings,

	"POST /api/export/tunnels":  TokenScopeTransfer,
	"POST /api/import/tunnels":  TokenScopeTransfer,
	"POST /api/export/settings": TokenScopeTransfer,
	"POST /api/import/settings": TokenScopeTransfer,

	"POST /api/restart":        TokenScopeOperations,
	"POST /api/update/check":   TokenScopeOperations,
	"POST /api/update/install": TokenScopeOperations,
	"POST /api/uninstall":      TokenScopeOperations,
	"POST /api/logs/clear":     TokenScopeOperations,
}

// tokenNeverRoutes are the routes no scope opens. What they change is the
// credentials themselves, or they are how a session is had: a token that could
// reach them could make itself another token, or a session that outlives it.
var tokenNeverRoutes = map[string]bool{
	"POST /api/logout":      true,
	"POST /api/setup":       true,
	"PUT /api/account":      true,
	"GET /api/token":        true,
	"POST /api/token":       true,
	"DELETE /api/token/:id": true,
}

// tokenPublicRoutes are answered by RequireSession before it looks for any
// credential, so a token sent to them is never read.
var tokenPublicRoutes = map[string]bool{
	"POST " + loginPath: true,
	"GET " + setupPath:  true,
}

// TokenRouteRule says what a token needs to reach method on route, where route
// is the path as echo registers it (/api/host/:id). It answers a scope,
// TokenRouteNever, TokenRoutePublic, or "" for a route none of the tables
// names, which is refused to a token like a TokenRouteNever one is.
func TokenRouteRule(method, route string) string {
	key := method + " " + route

	if tokenPublicRoutes[key] {
		return TokenRoutePublic
	}

	if tokenNeverRoutes[key] {
		return TokenRouteNever
	}

	return tokenRouteScopes[key]
}

// TokenRoutes is every route the three tables name, as "METHOD route", so that
// a test can hold them to the routes that are registered.
func TokenRoutes() []string {
	var routes []string

	for _, table := range []map[string]bool{tokenPublicRoutes, tokenNeverRoutes} {
		for route := range table {
			routes = append(routes, route)
		}
	}

	for route := range tokenRouteScopes {
		routes = append(routes, route)
	}

	sort.Strings(routes)

	return routes
}

// isTokenScope reports whether name is one of the scopes.
func isTokenScope(name string) bool {
	for _, scope := range tokenScopes {
		if scope == name {
			return true
		}
	}

	return false
}

// splitTokenScopes reads the stored list back into its scopes.
func splitTokenScopes(stored string) []string {
	if stored == "" {
		return []string{}
	}

	return strings.Split(stored, ",")
}

// tokenHasScope reports whether a stored list holds scope.
func tokenHasScope(stored, scope string) bool {
	for _, held := range splitTokenScopes(stored) {
		if held == scope {
			return true
		}
	}

	return false
}

// hashAPIToken is what a token is stored and looked up as. SHA-256 and not
// bcrypt, because the token is 32 random bytes: models.APIToken says why a slow
// hash buys nothing for it.
func hashAPIToken(token string) string {
	sum := sha256.Sum256([]byte(token))

	return hex.EncodeToString(sum[:])
}

// newAPIToken makes a token. The randomness is read the way a session token's
// is, and the prefix goes in front of it.
func newAPIToken() (string, error) {
	random, err := randomToken(apiTokenBytes)
	if err != nil {
		return "", err
	}

	return apiTokenPrefix + random, nil
}

// bearerToken returns the token of an Authorization: Bearer header and reports
// whether the request carries one. The scheme is compared without regard to
// case, which is how RFC 9110 says a scheme is read. A header of another
// scheme is not a token and is left alone: the request is then served as one
// that brought no header at all.
func bearerToken(c echo.Context) (string, bool) {
	header := c.Request().Header.Get(echo.HeaderAuthorization)

	scheme, token, _ := strings.Cut(header, " ")
	if !strings.EqualFold(scheme, bearerScheme) {
		return "", false
	}

	return strings.TrimSpace(token), true
}

// serveToken is RequireSession for a request that came with a token.
//
// The token is looked up before the limiter is asked anything. A token that is
// found is served without the limiter being checked or touched: it is 32
// random bytes, so it is not something a counter has to protect, and a
// reservation held for as long as its handler runs would let a few slow or
// concurrent script requests fill the in-flight count of their address and
// hold the operator's login from that address. A held address does not block
// a good token either, for the same reason.
//
// A token that is not there is counted as a failed sign in against the address
// it came from, on the counters of the login, and the failure is recorded
// before the answer goes out. The token is too long to be guessed, so what the
// count stops is not the guessing so much as the noise, and the log it fills.
// Only the address is counted and not the account: the account counter holds
// every address at once, and a stranger sending bad tokens from many addresses
// would otherwise hold the operator's own login.
//
// A token that has run out is refused without being counted. It is a token
// this server made, so whoever sends it is not guessing, and a cron job left
// running with an old token would otherwise hold its host out of the login.
func (h *AuthHandler) serveToken(c echo.Context, next echo.HandlerFunc, token string) error {
	var found models.APIToken

	result := h.db.Where("hash = ?", hashAPIToken(token)).Limit(1).Find(&found)
	if result.Error != nil {
		h.logger.Error("failed to read the API tokens", logid.TokenReadFailed.Field(), zap.Error(result.Error))
		return failure(c, http.StatusInternalServerError, errTokenReadFailed)
	}

	if result.RowsAffected == 0 {
		return h.refuseUnknownToken(c)
	}

	now := time.Now()

	if found.ExpiresAt != nil && !now.Before(*found.ExpiresAt) {
		return failure(c, http.StatusUnauthorized, errAuthTokenExpired,
			errorArgs{"expires_at": found.ExpiresAt.UTC().Format(time.RFC3339)})
	}

	method := c.Request().Method
	route := c.Path()

	rule := TokenRouteRule(method, route)
	switch rule {
	case "", TokenRouteNever, TokenRoutePublic:
		return failure(c, http.StatusForbidden, errAuthTokenRouteRefused,
			errorArgs{"method": method, "path": route})
	}

	if !tokenHasScope(found.Scopes, rule) {
		return failure(c, http.StatusForbidden, errAuthTokenScopeMissing,
			errorArgs{"scope": rule, "method": method, "path": route})
	}

	// The account is read for the reason the session path reads it: the calls
	// that ask for the password again find it on the context. There is one
	// row, so the token names no account of its own.
	user, err := h.readUser()
	if err != nil {
		h.logger.Error("failed to read the account", logid.AccountReadFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errAccountReadFailed)
	}

	if user.SetupRequired {
		return failure(c, http.StatusForbidden, errAuthSetupRequired, errorArgs{"path": setupPath})
	}

	h.markTokenUsed(found, now)

	// A change is logged with the name of the token before it is made, so
	// that the line is there whatever the handler does with it. A read is not:
	// a script polling the status would bury every other line.
	if !isSafeMethod(method) {
		h.logger.Info("a change was requested with an API token",
			logid.TokenChangeRequested.Field(),
			zap.String("token", found.Name),
			zap.String("method", method),
			zap.String("path", c.Request().URL.Path))
	}

	c.Set(contextUserIDKey, user.ID)
	c.Set(contextPasswordLimiterKey, accountPasswordLimiter(h))

	return next(c)
}

// refuseUnknownToken answers a token that is not there, and counts it. The
// reservation is taken and turned into a failure in one call, so it is never
// held past this function. An address that is already held is answered the
// way a held login is, with Retry-After, and is not counted again.
func (h *AuthHandler) refuseUnknownToken(c echo.Context) error {
	attempt, wait, held := h.logins.beginAddress(h.loginAddress(c))
	if held {
		return h.refuseHeldLogin(c, wait)
	}

	attempt.failed()

	return failure(c, http.StatusUnauthorized, errAuthTokenInvalid)
}

// markTokenUsed writes last_used_at, unless it was written less than
// tokenLastUsedResolution ago. A write that fails is logged and the request is
// served all the same: the column is there for the operator to see which
// tokens are still in use, and not something the request depends on.
func (h *AuthHandler) markTokenUsed(found models.APIToken, now time.Time) {
	if found.LastUsedAt != nil && now.Sub(*found.LastUsedAt) < tokenLastUsedResolution {
		return
	}

	err := h.db.Model(&models.APIToken{}).Where("id = ?", found.ID).
		UpdateColumn("last_used_at", now).Error
	if err != nil {
		h.logger.Warn("failed to store when the API token was last used",
			logid.TokenLastUsedStoreFailed.Field(),
			zap.String("token", found.Name),
			zap.Error(err))
	}
}

// tokenView is a token as the Settings screen lists it. The hash is not part of
// it, and neither is the token: that is shown once, when it is made.
type tokenView struct {
	ID         uint       `json:"id"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

// tokenCreated is what the creation answers with: the token as it is listed,
// and the token itself, which is never handed out again.
type tokenCreated struct {
	ID         uint       `json:"id"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	Token      string     `json:"token"`
}

// tokenRequest is what the Settings screen sends to make a token.
//
// ExpiresInDays is one of 30, 90 and 365, or 0 for a token that never runs
// out. It is a pointer so that a request that leaves it out gets the default
// rather than a token that lives forever: 0 is the one value that has to be
// asked for.
type tokenRequest struct {
	Name          string   `json:"name"`
	Scopes        []string `json:"scopes"`
	ExpiresInDays *int     `json:"expires_in_days"`
}

// viewOfToken is the stored row as it is listed.
func viewOfToken(token models.APIToken) tokenView {
	return tokenView{
		ID:         token.ID,
		Name:       token.Name,
		Scopes:     splitTokenScopes(token.Scopes),
		CreatedAt:  token.CreatedAt,
		ExpiresAt:  token.ExpiresAt,
		LastUsedAt: token.LastUsedAt,
	}
}

// ListTokens answers with every token, oldest first.
//
// @Summary      List the API tokens
// @Description  Answers with the name, the scopes and the dates of every token. The token itself is never in it: it is shown once, when the token is made. Only a session reaches this; a token does not.
// @Tags         account
// @Produce  json
// @Success  200  {object}  models.Response{data=[]api.tokenView}
// @Failure  401  {object}  api.errorBody  "Authentication required"
// @Router       /token [get]
func (h *AuthHandler) ListTokens(c echo.Context) error {
	var tokens []models.APIToken

	err := h.db.Order("id").Find(&tokens).Error
	if err != nil {
		h.logger.Error("failed to read the API tokens", logid.TokenReadFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTokenReadFailed)
	}

	views := make([]tokenView, 0, len(tokens))
	for _, token := range tokens {
		views = append(views, viewOfToken(token))
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    views,
	})
}

// CreateToken makes a token and answers with it. This is the one answer the
// token is ever in: what is stored is its hash.
//
// @Summary      Make an API token
// @Description  Takes name, scopes and expires_in_days, and answers with the token in data.token. Save it: it is not shown again.
// @Description  The scopes are read, hosts, tunnels, host-keys, settings, transfer and operations. expires_in_days is 30, 90 or 365, or 0 for a token that never runs out, and is 90 when it is left out.
// @Description  Send the token as Authorization: Bearer <token>. A request that does needs no session and no X-CSRF-Token.
// @Description  Only a session reaches this; a token does not.
// @Tags         account
// @Accept   json
// @Produce  json
// @Security  CSRFToken
// @Param   body  body  api.tokenRequest  true  "The name, the scopes and how long the token lives"
// @Success  201  {object}  models.Response{data=api.tokenCreated}
// @Failure  400  {object}  api.errorBody  "The name, a scope or the lifetime is refused"
// @Failure  409  {object}  api.errorBody  "There is a token of that name already"
// @Router       /token [post]
func (h *AuthHandler) CreateToken(c echo.Context) error {
	var req tokenRequest

	err := c.Bind(&req)
	if err != nil {
		return failure(c, http.StatusBadRequest, errTokenRequestInvalid)
	}

	name := strings.TrimSpace(req.Name)

	switch {
	case name == "":
		return failure(c, http.StatusBadRequest, errTokenNameEmpty)
	case utf8.RuneCountInString(name) > tokenNameMaxLength:
		return failure(c, http.StatusBadRequest, errTokenNameTooLong,
			errorArgs{"max": strconv.Itoa(tokenNameMaxLength)})
	}

	if len(req.Scopes) == 0 {
		return failure(c, http.StatusBadRequest, errTokenScopesEmpty)
	}

	asked := map[string]bool{}

	for _, scope := range req.Scopes {
		if !isTokenScope(scope) {
			return failure(c, http.StatusBadRequest, errTokenScopeUnknown,
				errorArgs{"scope": scope, "scopes": strings.Join(tokenScopes, ", ")})
		}

		asked[scope] = true
	}

	// The scopes are stored in the order of tokenScopes whatever order they
	// came in, and each of them once.
	var scopes []string

	for _, scope := range tokenScopes {
		if asked[scope] {
			scopes = append(scopes, scope)
		}
	}

	days := tokenDefaultExpiryDays
	if req.ExpiresInDays != nil {
		days = *req.ExpiresInDays
	}

	if !tokenExpiryAllowed(days) {
		return failure(c, http.StatusBadRequest, errTokenExpiryUnsupported,
			errorArgs{"days": tokenExpiryChoices(), "value": strconv.Itoa(days)})
	}

	secret, err := newAPIToken()
	if err != nil {
		h.logger.Error("failed to create an API token", logid.TokenCreateFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTokenCreateFailed)
	}

	now := time.Now()

	row := models.APIToken{
		Name:      name,
		Hash:      hashAPIToken(secret),
		Scopes:    strings.Join(scopes, ","),
		CreatedAt: now,
	}

	if days != 0 {
		expires := now.AddDate(0, 0, days)
		row.ExpiresAt = &expires
	}

	tx := h.db.Begin()
	err = tx.Error
	if err != nil {
		h.logger.Error("failed to start the transaction", logid.DatabaseTransactionStartFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionBeginFailed)
	}
	defer rollbackUnlessDone(tx)

	// The name is looked for inside the transaction, so that two requests for
	// the same name are answered one after the other: the pool holds one
	// connection, and the second finds the row the first one wrote. The unique
	// index is behind it, and would turn the second into a failure to store
	// rather than a refusal that names the name.
	var taken int64

	err = tx.Model(&models.APIToken{}).Where("name = ?", name).Count(&taken).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to read the API tokens", logid.TokenReadFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTokenReadFailed)
	}

	if taken > 0 {
		tx.Rollback()
		return failure(c, http.StatusConflict, errTokenNameTaken, errorArgs{"name": name})
	}

	err = tx.Create(&row).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to create an API token", logid.TokenCreateFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTokenCreateFailed)
	}

	err = tx.Commit().Error
	if err != nil {
		h.logger.Error("failed to commit the transaction", logid.DatabaseTransactionCommitFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionCommitFailed)
	}

	h.logger.Info("an API token was made",
		logid.TokenCreated.Field(),
		zap.String("token", row.Name),
		zap.String("scopes", row.Scopes))

	view := viewOfToken(row)

	return c.JSON(http.StatusCreated, models.Response{
		Success: true,
		Data: tokenCreated{
			ID:         view.ID,
			Name:       view.Name,
			Scopes:     view.Scopes,
			CreatedAt:  view.CreatedAt,
			ExpiresAt:  view.ExpiresAt,
			LastUsedAt: view.LastUsedAt,
			Token:      secret,
		},
	})
}

// tokenExpiryAllowed reports whether days is one of the lifetimes a token can
// be made with.
func tokenExpiryAllowed(days int) bool {
	for _, allowed := range tokenExpiryDays {
		if allowed == days {
			return true
		}
	}

	return false
}

// tokenExpiryChoices is the lifetimes as a refusal names them.
func tokenExpiryChoices() string {
	choices := make([]string, 0, len(tokenExpiryDays))
	for _, days := range tokenExpiryDays {
		choices = append(choices, strconv.Itoa(days))
	}

	return strings.Join(choices, ", ")
}

// DeleteToken revokes a token. The row goes, so the next request that brings
// the token is answered as one that brings a token this server never made.
//
// @Summary      Revoke an API token
// @Description  Only a session reaches this; a token does not.
// @Tags         account
// @Produce  json
// @Security  CSRFToken
// @Param   id  path  int  true  "The id of the token"
// @Success  200  {object}  models.Response
// @Failure  404  {object}  api.errorBody  "No such token"
// @Router       /token/{id} [delete]
func (h *AuthHandler) DeleteToken(c echo.Context) error {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		return failure(c, http.StatusBadRequest, errTokenIDInvalid, errorArgs{"reason": err.Error()})
	}

	var found models.APIToken

	err = h.db.First(&found, id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return failure(c, http.StatusNotFound, errTokenNotFound)
		}
		h.logger.Error("failed to read the API tokens", logid.TokenReadFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTokenReadFailed)
	}

	result := h.db.Delete(&models.APIToken{}, id)
	if result.Error != nil {
		h.logger.Error("failed to revoke an API token", logid.TokenRevokeFailed.Field(),
			zap.String("token", found.Name), zap.Error(result.Error))
		return failure(c, http.StatusInternalServerError, errTokenDeleteFailed)
	}

	if result.RowsAffected == 0 {
		return failure(c, http.StatusNotFound, errTokenNotFound)
	}

	h.logger.Info("an API token was revoked", logid.TokenRevoked.Field(), zap.String("token", found.Name))

	return c.JSON(http.StatusOK, models.Response{Success: true})
}
