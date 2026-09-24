package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/settings"
	"github.com/jollaman999/tunnel-manager/internal/tunnel"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// socksLogin is what the Hosts of these tests are registered to log in with.
// It is handed to json.Marshal rather than written into a body, so that no
// line of this file reads as a credential.
const socksLogin = "fake-value-1"

// socksHost is a Host whose SOCKS5 proxy is switched on at port.
func socksHost(id uint, enabled bool, port int) models.Host {
	host := statusHost(id, enabled)
	host.SocksEnabled = true
	host.SocksPort = port

	return host
}

// newSocksDB is newLocalForwardDB with the SOCKS5 fields of each Host written
// by name, for the reason the enabled flag is written that way there: a false
// is left out of the insert, and a Host stored with its proxy on stays on.
func newSocksDB(t *testing.T, hosts []models.Host, forwards []models.LocalForward) *gorm.DB {
	t.Helper()

	db := newLocalForwardDB(t, hosts, forwards)

	for _, host := range hosts {
		err := db.Model(&models.Host{}).Where("id = ?", host.ID).Updates(map[string]interface{}{
			"socks_enabled":         host.SocksEnabled,
			"socks_port":            host.SocksPort,
			"socks_bind_scope":      host.SocksBindScope,
			"socks_allowed_sources": host.SocksAllowedSources,
		}).Error
		if err != nil {
			t.Fatalf("failed to store the SOCKS5 proxy of a Host: %v", err)
		}
	}

	return db
}

// createHostBody is the body of a create with the SOCKS5 fields given.
func createHostBody(t *testing.T, ip string, socks map[string]interface{}) string {
	t.Helper()

	body := map[string]interface{}{
		"ip":                       ip,
		"port":                     22,
		"user":                     "root",
		"password":                 socksLogin,
		"assign_all_service_ports": false,
	}
	for name, value := range socks {
		body[name] = value
	}

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("failed to write the body: %v", err)
	}

	return string(raw)
}

// hostAnswer is a success carrying one Host.
type hostAnswer struct {
	Success bool     `json:"success"`
	Data    hostView `json:"data"`
}

func readHostAnswer(t *testing.T, rec *httptest.ResponseRecorder) hostView {
	t.Helper()

	var answer hostAnswer

	err := json.Unmarshal(rec.Body.Bytes(), &answer)
	if err != nil {
		t.Fatalf("failed to read the answer: %v, body: %s", err, rec.Body.String())
	}

	if !answer.Success {
		t.Fatalf("success = false, body: %s", rec.Body.String())
	}

	return answer.Data
}

func storedHostByIP(t *testing.T, db *gorm.DB, ip string) (models.Host, bool) {
	t.Helper()

	var host models.Host

	err := db.Where("ip = ?", ip).Limit(1).Find(&host).Error
	if err != nil {
		t.Fatalf("failed to read the Host %s: %v", ip, err)
	}

	return host, host.ID != 0
}

// TestCreateHostStoresTheSocksProxy pins a create that switches the proxy on:
// the four fields are stored, an empty scope as the wildcard, and the answer
// carries them with the status of a proxy nothing runs for yet.
func TestCreateHostStoresTheSocksProxy(t *testing.T) {
	db := newSocksDB(t, []models.Host{statusHost(1, true)}, nil)
	h := NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.NewNop(), newTestCipher(t))

	c, rec := createRequest(t, "/api/host", createHostBody(t, "192.0.2.50", map[string]interface{}{
		"socks_enabled":         true,
		"socks_port":            1080,
		"socks_allowed_sources": "192.0.2.0/24, 198.51.100.7",
	}))

	err := h.CreateHost(c)
	if err != nil {
		t.Fatalf("CreateHost returned error: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	view := readHostAnswer(t, rec)
	if !view.SocksEnabled || view.SocksPort != 1080 || view.SocksBindScope != models.BindScopeWildcard ||
		view.SocksAllowedSources != "192.0.2.0/24, 198.51.100.7" {
		t.Errorf("answer = %+v, want the proxy that was sent on the wildcard", view)
	}
	if view.SocksStatus != socksStatusStopped {
		t.Errorf("socks_status = %q, want %q", view.SocksStatus, socksStatusStopped)
	}

	stored, found := storedHostByIP(t, db, "192.0.2.50")
	if !found || !stored.SocksEnabled || stored.SocksPort != 1080 ||
		stored.SocksBindScope != models.BindScopeWildcard {
		t.Errorf("stored = %+v, want the proxy on 1080 on the wildcard", stored)
	}
}

// TestCreateHostRefusesASocksProxyItCannotOpen is every refusal of a create
// over the proxy. Each leaves nothing stored.
func TestCreateHostRefusesASocksProxyItCannotOpen(t *testing.T) {
	tests := []struct {
		name   string
		socks  map[string]interface{}
		status int
		code   errorCode
		args   errorArgs
	}{
		{"switched on without a port", map[string]interface{}{"socks_enabled": true},
			http.StatusBadRequest, errHostSocksPortRequired, nil},
		{"a port out of range", map[string]interface{}{"socks_enabled": true, "socks_port": 70000},
			http.StatusBadRequest, errRequestValidationFailed, nil},
		{"a scope that is neither", map[string]interface{}{"socks_enabled": true, "socks_port": 1080,
			"socks_bind_scope": "everywhere"},
			http.StatusBadRequest, errRequestValidationFailed, nil},
		{"allowed sources that are not addresses", map[string]interface{}{"socks_enabled": true,
			"socks_port": 1080, "socks_allowed_sources": "192.0.2.0/24, not-an-address"},
			http.StatusBadRequest, errHostSocksSourcesInvalid, nil},
		{"the port of this server", map[string]interface{}{"socks_enabled": true,
			"socks_port": localForwardAPIPort},
			http.StatusConflict, errHostSocksPortIsAPIPort, errorArgs{"socks_port": "19443"}},
		{"the running port of this server", map[string]interface{}{"socks_enabled": true,
			"socks_port": 19444},
			http.StatusConflict, errHostSocksPortIsAPIPort, errorArgs{"socks_port": "19444"}},
		{"the port of another proxy", map[string]interface{}{"socks_enabled": true, "socks_port": 1080},
			http.StatusConflict, errHostSocksPortTaken, errorArgs{"socks_port": "1080", "host": "192.0.2.1"}},
		{"the port of a local forward", map[string]interface{}{"socks_enabled": true, "socks_port": 15432},
			http.StatusConflict, errHostSocksPortLocalForward,
			errorArgs{"socks_port": "15432", "host": "192.0.2.2"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := newSocksDB(t, []models.Host{socksHost(1, true, 1080), statusHost(2, true)},
				[]models.LocalForward{storedLocalForward(1, 2, 15432)})
			h := NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.NewNop(), newTestCipher(t))
			h.SetRunningAPIPort(19444)

			c, rec := createRequest(t, "/api/host", createHostBody(t, "192.0.2.50", tc.socks))

			err := h.CreateHost(c)
			if err != nil {
				t.Fatalf("CreateHost returned error: %v", err)
			}
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, tc.status, rec.Body.String())
			}

			var body errorBody

			err = json.Unmarshal(rec.Body.Bytes(), &body)
			if err != nil {
				t.Fatalf("failed to read the refusal: %v", err)
			}
			if body.Code != tc.code {
				t.Errorf("error_code = %q, want %q, body: %s", body.Code, tc.code, rec.Body.String())
			}
			for name, want := range tc.args {
				if body.Args[name] != want {
					t.Errorf("error_args[%s] = %q, want %q", name, body.Args[name], want)
				}
			}

			if _, found := storedHostByIP(t, db, "192.0.2.50"); found {
				t.Errorf("the refused Host was stored")
			}
		})
	}
}

// TestCreateHostTakesAProxyThatIsOffOnATakenPort is the other side: a proxy
// that is switched off opens nothing, so its port is held to nothing.
func TestCreateHostTakesAProxyThatIsOffOnATakenPort(t *testing.T) {
	db := newSocksDB(t, []models.Host{socksHost(1, true, 1080)}, nil)
	h := NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.NewNop(), newTestCipher(t))

	c, rec := createRequest(t, "/api/host", createHostBody(t, "192.0.2.50", map[string]interface{}{
		"socks_enabled": false,
		"socks_port":    1080,
	}))

	err := h.CreateHost(c)
	if err != nil {
		t.Fatalf("CreateHost returned error: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	if view := readHostAnswer(t, rec); view.SocksStatus != socksStatusOff {
		t.Errorf("socks_status = %q, want %q", view.SocksStatus, socksStatusOff)
	}
}

// updateHost runs one update of Host 1.
func updateHost(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()

	c, rec := localForwardRequest(t, http.MethodPut, "/api/host/1", body, "1")

	err := h.UpdateHost(c)
	if err != nil {
		t.Fatalf("UpdateHost returned error: %v", err)
	}

	return rec
}

// TestUpdateHostChangesTheSocksProxy pins an update of the proxy, and that a
// field the request leaves out keeps what is stored, the allowed sources
// included, while one sent empty clears them.
func TestUpdateHostChangesTheSocksProxy(t *testing.T) {
	host := socksHost(1, true, 1080)
	host.SocksBindScope = models.BindScopeLoopback
	host.SocksAllowedSources = "192.0.2.0/24"

	db := newSocksDB(t, []models.Host{host}, nil)
	h := NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.NewNop(), newTestCipher(t))

	rec := updateHost(t, h, `{"description":"moved","socks_port":1081}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	stored, _ := storedHostByIP(t, db, "192.0.2.1")
	if !stored.SocksEnabled || stored.SocksPort != 1081 || stored.SocksBindScope != models.BindScopeLoopback ||
		stored.SocksAllowedSources != "192.0.2.0/24" {
		t.Errorf("stored = %+v, want the port moved and the rest kept", stored)
	}

	rec = updateHost(t, h, `{"socks_allowed_sources":"","socks_bind_scope":"wildcard"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	stored, _ = storedHostByIP(t, db, "192.0.2.1")
	if stored.SocksAllowedSources != "" || stored.SocksBindScope != models.BindScopeWildcard {
		t.Errorf("stored = %+v, want every source let in on the wildcard", stored)
	}

	rec = updateHost(t, h, `{"socks_enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if view := readHostAnswer(t, rec); view.SocksEnabled || view.SocksPort != 1081 ||
		view.SocksStatus != socksStatusOff {
		t.Errorf("answer = %+v, want the proxy off with its port kept", view)
	}
}

// TestUpdateHostRefusesASocksProxyItCannotOpen is every refusal of an update
// over the proxy. Each leaves the row as it was.
func TestUpdateHostRefusesASocksProxyItCannotOpen(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		status int
		code   errorCode
	}{
		{"switched on without a port", `{"socks_enabled":true}`, http.StatusBadRequest, errHostSocksPortRequired},
		{"allowed sources that are not addresses", `{"socks_allowed_sources":"192.0.2.0/33"}`,
			http.StatusBadRequest, errHostSocksSourcesInvalid},
		{"a scope that is neither", `{"socks_bind_scope":"everywhere"}`,
			http.StatusBadRequest, errRequestValidationFailed},
		{"the port of this server", `{"socks_enabled":true,"socks_port":19443}`,
			http.StatusConflict, errHostSocksPortIsAPIPort},
		{"the port of another proxy", `{"socks_enabled":true,"socks_port":1080}`,
			http.StatusConflict, errHostSocksPortTaken},
		{"the port of a local forward", `{"socks_enabled":true,"socks_port":15432}`,
			http.StatusConflict, errHostSocksPortLocalForward},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := newSocksDB(t, []models.Host{statusHost(1, true), socksHost(2, false, 1080)},
				[]models.LocalForward{storedLocalForward(1, 2, 15432)})
			h := NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.NewNop(), newTestCipher(t))

			rec := updateHost(t, h, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, tc.status, rec.Body.String())
			}
			if code := readRefusalCode(t, rec); code != string(tc.code) {
				t.Errorf("error_code = %q, want %q", code, tc.code)
			}

			stored, _ := storedHostByIP(t, db, "192.0.2.1")
			if stored.SocksEnabled || stored.SocksPort != 0 || stored.SocksAllowedSources != "" {
				t.Errorf("stored = %+v, want the row as it was", stored)
			}
		})
	}
}

// TestUpdateHostKeepsAProxyOnThePortItHas is a change of another field of a
// Host whose proxy is stored on its port: the row meets itself and not
// another proxy.
func TestUpdateHostKeepsAProxyOnThePortItHas(t *testing.T) {
	db := newSocksDB(t, []models.Host{socksHost(1, true, 1080)}, nil)
	h := NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.NewNop(), newTestCipher(t))

	rec := updateHost(t, h, `{"socks_enabled":true,"socks_port":1080,"description":"same port"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

// TestTheHostViewCarriesTheSocksStatus pins each status a Host answers with.
func TestTheHostViewCarriesTheSocksStatus(t *testing.T) {
	states := map[uint]tunnel.SocksState{
		1: {Status: "error", LastError: "listen tcp :1080: bind: address already in use"},
		4: {Status: "connected"},
	}

	tests := []struct {
		host      models.Host
		status    string
		lastError string
	}{
		{socksHost(1, true, 1080), "error", "listen tcp :1080: bind: address already in use"},
		{socksHost(2, true, 1081), socksStatusStopped, ""},
		{socksHost(3, false, 1082), socksStatusDisabled, ""},
		{statusHost(4, true), socksStatusOff, ""},
	}

	for _, tc := range tests {
		view := hostViewOf(tc.host, states)
		if view.SocksStatus != tc.status || view.SocksLastError != tc.lastError {
			t.Errorf("Host %d: socks_status = %q, socks_last_error = %q, want %q and %q", tc.host.ID,
				view.SocksStatus, view.SocksLastError, tc.status, tc.lastError)
		}
		if view.SocksBindScope != models.BindScopeWildcard {
			t.Errorf("Host %d: socks_bind_scope = %q, want %q", tc.host.ID, view.SocksBindScope,
				models.BindScopeWildcard)
		}
	}
}

// TestALocalForwardOnTheSocksPortIsRefused is the other side of the proxy's
// own check: a local forward on the port a proxy that is switched on opens,
// whether its Host is enabled or not, is a conflict naming that Host.
func TestALocalForwardOnTheSocksPortIsRefused(t *testing.T) {
	db := newSocksDB(t, []models.Host{statusHost(1, true), socksHost(2, false, 1080)},
		[]models.LocalForward{storedLocalForward(1, 1, 15001)})
	h := NewHandler(db, &wakeRecorder{tx: &txConnPool{}}, zap.NewNop(), newTestCipher(t))

	c, rec := localForwardRequest(t, http.MethodPost, "/api/host/1/local-forward",
		`{"local_port":1080,"target_ip":"127.0.0.1","target_port":5432}`, "1")

	err := h.CreateHostLocalForward(c)
	if err != nil {
		t.Fatalf("CreateHostLocalForward returned error: %v", err)
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("create: status = %d, want %d, body: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}

	var body errorBody

	err = json.Unmarshal(rec.Body.Bytes(), &body)
	if err != nil {
		t.Fatalf("failed to read the refusal: %v", err)
	}
	if body.Code != errLocalForwardPortSocks || body.Args["host"] != "192.0.2.2" {
		t.Errorf("create: error_code = %q, error_args = %v, want %q naming 192.0.2.2", body.Code, body.Args,
			errLocalForwardPortSocks)
	}

	c, rec = localForwardRowRequest(t, http.MethodPut, "/api/host/1/local-forward/1",
		`{"local_port":1080,"target_ip":"127.0.0.1","target_port":5432}`, "1", "1")

	err = h.UpdateLocalForward(c)
	if err != nil {
		t.Fatalf("UpdateLocalForward returned error: %v", err)
	}
	if rec.Code != http.StatusConflict || readRefusalCode(t, rec) != string(errLocalForwardPortSocks) {
		t.Errorf("update: status = %d, body: %s, want %d under %q", rec.Code, rec.Body.String(),
			http.StatusConflict, errLocalForwardPortSocks)
	}

	rows := storedLocalForwards(t, db)
	if len(rows) != 1 || rows[0].LocalPort != 15001 {
		t.Errorf("stored = %+v, want the one forward as it was", rows)
	}
}

// TestSaveRefusesAnAPIPortASocksProxyOpens pins the refusal of a port the
// proxy of a Host opens: a 409 under a code of its own, the proxy in
// socks_host, the local_forward object still there, and a suggested port that
// steps over the proxies as well as the forwards.
func TestSaveRefusesAnAPIPortASocksProxyOpens(t *testing.T) {
	db := newSocksDB(t, []models.Host{socksHost(1, true, 15432), socksHost(2, false, 15433)},
		[]models.LocalForward{storedLocalForward(1, 1, 15434)})
	h, _, _, _ := newSettingsHandler(t, db)

	rec := settingsRequest(t, h, `{"api_port":15432}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}

	answer := readAPIPortTaken(t, rec)
	if answer.Code != string(errSettingsAPIPortSocks) {
		t.Errorf("error_code = %q, want %q", answer.Code, errSettingsAPIPortSocks)
	}

	want := apiPortSocksHolder{HostID: 1, HostIP: "192.0.2.1", SocksPort: 15432}
	if answer.Data.SocksHost == nil || *answer.Data.SocksHost != want {
		t.Errorf("socks_host = %+v, want %+v", answer.Data.SocksHost, want)
	}

	if answer.Data.LocalForward != (apiPortHolder{}) {
		t.Errorf("local_forward = %+v, want it empty", answer.Data.LocalForward)
	}
	if !strings.Contains(rec.Body.String(), `"local_forward":{`) {
		t.Errorf("the answer carries no local_forward object, body: %s", rec.Body.String())
	}

	// 15433 is the proxy of the disabled Host and 15434 a local forward.
	if answer.Data.SuggestedPort != 15435 {
		t.Errorf("suggested_port = %d, want 15435", answer.Data.SuggestedPort)
	}

	if answer.Args["api_port"] != "15432" || answer.Args["host"] != "192.0.2.1" {
		t.Errorf("error_args = %v", answer.Args)
	}

	after, err := settings.Load(db)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}
	if after.APIPort != localForwardAPIPort {
		t.Errorf("the refused save was stored: api_port %d", after.APIPort)
	}
}

// TestTheSuggestedPortStepsOverTheSocksProxies is the refusal of a port a
// local forward opens: the port suggested past it steps over a proxy that is
// switched on and not over one that is off.
func TestTheSuggestedPortStepsOverTheSocksProxies(t *testing.T) {
	off := statusHost(2, true)
	off.SocksPort = 15434

	db := newSocksDB(t, []models.Host{socksHost(1, true, 15433), off},
		[]models.LocalForward{storedLocalForward(1, 1, 15432)})
	h, _, _, _ := newSettingsHandler(t, db)

	rec := settingsRequest(t, h, `{"api_port":15432}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}

	answer := readAPIPortTaken(t, rec)
	if answer.Code != string(errSettingsAPIPortLocalForward) {
		t.Errorf("error_code = %q, want %q", answer.Code, errSettingsAPIPortLocalForward)
	}
	if answer.Data.SocksHost != nil {
		t.Errorf("socks_host = %+v, want none for a local forward", answer.Data.SocksHost)
	}
	if answer.Data.SuggestedPort != 15434 {
		t.Errorf("suggested_port = %d, want 15434, past the proxy on 15433", answer.Data.SuggestedPort)
	}
}

// withSocks is a Host of a file that switches its proxy on at port.
func withSocks(host hostContent, port int, scope string, sources string) hostContent {
	enabled := true
	host.SocksEnabled = &enabled
	host.SocksPort = &port
	host.SocksBindScope = &scope
	host.SocksAllowedSources = &sources

	return host
}

// setSocks switches the proxy of a registered Host on at port.
func (i *transferInstall) setSocks(t *testing.T, hostIP string, port int, scope string, sources string) {
	t.Helper()

	err := i.db.Model(&models.Host{}).Where("ip = ?", hostIP).Updates(map[string]interface{}{
		"socks_enabled":         true,
		"socks_port":            port,
		"socks_bind_scope":      scope,
		"socks_allowed_sources": sources,
	}).Error
	if err != nil {
		t.Fatalf("failed to store the SOCKS5 proxy of %s: %v", hostIP, err)
	}
}

// proxies is the proxy of every Host stored, as "<Host IP> <on> <port> <scope>
// <sources>".
func (i *transferInstall) proxies(t *testing.T) []string {
	t.Helper()

	var hosts []models.Host

	err := i.db.Order("ip").Find(&hosts).Error
	if err != nil {
		t.Fatalf("failed to read the Hosts: %v", err)
	}

	named := make([]string, 0, len(hosts))
	for _, host := range hosts {
		named = append(named, strings.Join([]string{host.IP, map[bool]string{true: "on", false: "off"}[host.SocksEnabled],
			strconv.Itoa(host.SocksPort), host.SocksBindScope, host.SocksAllowedSources}, " "))
	}

	return named
}

// TestTheSocksProxyOfAHostCrossesToAnotherInstallation is the round trip: the
// four fields are written into the file and stored by the import, and the
// proxy is an item of its own, named by its code.
func TestTheSocksProxyOfAHostCrossesToAnotherInstallation(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	source.registerHost(t, passwordHost("192.0.2.10"))
	source.registerHost(t, passwordHost("192.0.2.11"))
	source.setSocks(t, "192.0.2.10", 1080, models.BindScopeLoopback, "192.0.2.0/24 198.51.100.7")

	rec := target.importTunnels(t, source.exportTunnels(t, testExportPassword), testExportPassword, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	want := []string{
		"192.0.2.10 on 1080 loopback 192.0.2.0/24 198.51.100.7",
		"192.0.2.11 off 0 wildcard ",
	}
	if !reflect.DeepEqual(target.proxies(t), want) {
		t.Fatalf("the imported proxies are %v, want %v", target.proxies(t), want)
	}

	checkNamedItems(t, decodeTransfer(t, rec).Data, []namedItem{{
		kind:       "socks",
		name:       "192.0.2.10 opens the SOCKS5 proxy on 1080",
		nameCode:   textImportNameSocks,
		nameValues: textArgs{"host": "192.0.2.10", "socks_port": "1080"},
		action:     transferAdded,
	}})
}

// TestAFileFromBeforeTheSocksProxiesLeavesThemAlone is a file with none of
// the four fields imported with overwrite onto a Host whose proxy is on here.
// Such a file says nothing about the proxy, so it stays as it is; a file that
// carries the proxy as off switches it off.
func TestAFileFromBeforeTheSocksProxiesLeavesThemAlone(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	source.registerHost(t, passwordHost("192.0.2.10"))

	target.registerHost(t, passwordHost("192.0.2.10"))
	target.setSocks(t, "192.0.2.10", 1080, models.BindScopeWildcard, "192.0.2.0/24")

	before := target.proxies(t)
	exported := source.exportTunnels(t, testExportPassword)

	file := rewriteHosts(t, exported, testExportPassword, func(host map[string]interface{}) {
		for _, name := range []string{"socks_enabled", "socks_port", "socks_bind_scope", "socks_allowed_sources"} {
			delete(host, name)
		}
	})

	rec := target.importTunnels(t, file, testExportPassword, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	if !reflect.DeepEqual(target.proxies(t), before) {
		t.Fatalf("a file that names no proxy changed it to %v, want %v", target.proxies(t), before)
	}

	rec = target.importTunnels(t, exported, testExportPassword, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	want := []string{"192.0.2.10 off 0 wildcard "}
	if !reflect.DeepEqual(target.proxies(t), want) {
		t.Fatalf("a file that carries the proxy as off left %v, want %v", target.proxies(t), want)
	}
}

// TestASocksProxyTheInstallationCannotOpenIsRefused is every refusal of an
// import over a proxy. Each takes the whole import back out.
func TestASocksProxyTheInstallationCannotOpenIsRefused(t *testing.T) {
	const apiPort = 19443

	tests := []struct {
		name   string
		hosts  func() []hostContent
		status int
		code   errorCode
	}{
		{"on the port of this server", func() []hostContent {
			return []hostContent{withSocks(passwordHost("192.0.2.10"), apiPort, "", "")}
		}, http.StatusConflict, errImportSocksAPIPort},
		{"on the port of a proxy here", func() []hostContent {
			return []hostContent{withSocks(passwordHost("192.0.2.10"), 1080, "", "")}
		}, http.StatusConflict, errImportSocksPortTaken},
		{"on the port of a local forward here", func() []hostContent {
			return []hostContent{withSocks(passwordHost("192.0.2.10"), 15432, "", "")}
		}, http.StatusConflict, errImportSocksLocalForward},
		{"a local forward on the port of a proxy here", func() []hostContent {
			host := passwordHost("192.0.2.10")
			host.LocalForwards = []localForwardContent{{LocalPort: 1080, TargetIP: "192.0.2.30", TargetPort: 80}}
			return []hostContent{host}
		}, http.StatusConflict, errImportLocalForwardSocks},
		{"two proxies of the file on one port", func() []hostContent {
			return []hostContent{
				withSocks(passwordHost("192.0.2.10"), 1090, "", ""),
				withSocks(passwordHost("192.0.2.11"), 1090, "", ""),
			}
		}, http.StatusBadRequest, errImportSocksDuplicate},
		{"a proxy and a local forward of the file on one port", func() []hostContent {
			host := withSocks(passwordHost("192.0.2.10"), 1090, "", "")
			other := passwordHost("192.0.2.11")
			other.LocalForwards = []localForwardContent{{LocalPort: 1090, TargetIP: "192.0.2.30", TargetPort: 80}}
			return []hostContent{host, other}
		}, http.StatusBadRequest, errImportSocksDuplicate},
		{"allowed sources that are not addresses", func() []hostContent {
			return []hostContent{withSocks(passwordHost("192.0.2.10"), 1090, "", "somewhere")}
		}, http.StatusBadRequest, errImportHostRefused},
		{"a scope that is neither", func() []hostContent {
			return []hostContent{withSocks(passwordHost("192.0.2.10"), 1090, "everywhere", "")}
		}, http.StatusBadRequest, errImportHostRefused},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			source := newTransferInstall(t)
			target := newTransferInstall(t)

			stored := settings.Defaults()
			stored.APIPort = apiPort

			err := settings.Save(target.db, &stored)
			if err != nil {
				t.Fatalf("failed to store the settings: %v", err)
			}

			target.registerHost(t, passwordHost("192.0.2.12"))
			target.setSocks(t, "192.0.2.12", 1080, models.BindScopeWildcard, "")
			target.forward(t, "192.0.2.12", localForwardContent{BindScope: models.BindScopeWildcard,
				LocalPort: 15432, TargetIP: "192.0.2.40", TargetPort: 5432})

			before := target.proxies(t)

			file := sealedTunnelsFile(t, source, tunnelsContent{Hosts: tc.hosts()}, testExportPassword)

			rec := target.importTunnels(t, file, testExportPassword, true)
			if rec.Code != tc.status {
				t.Fatalf("the import answered %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}

			if errorCodeOf(t, rec) != tc.code {
				t.Errorf("the import was refused under %s, want %s: %s", errorCodeOf(t, rec), tc.code,
					rec.Body.String())
			}

			if !reflect.DeepEqual(target.proxies(t), before) || target.count(t, &models.LocalForward{}) != 1 {
				t.Fatalf("the refused import left %v and %d local forwards, want %v and 1",
					target.proxies(t), target.count(t, &models.LocalForward{}), before)
			}
		})
	}
}

// TestASocksPortTheFileMovesIsFree is a proxy the file moves onto the port a
// Host of the same file gives up here: the Hosts are written first, so the
// port is free by the time it is held to the rest.
func TestASocksPortTheFileMovesIsFree(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	target.registerHost(t, passwordHost("192.0.2.10"))
	target.registerHost(t, passwordHost("192.0.2.11"))
	target.setSocks(t, "192.0.2.10", 1080, models.BindScopeWildcard, "")
	target.setSocks(t, "192.0.2.11", 1081, models.BindScopeWildcard, "")

	file := sealedTunnelsFile(t, source, tunnelsContent{Hosts: []hostContent{
		withSocks(passwordHost("192.0.2.10"), 1081, models.BindScopeWildcard, ""),
		withSocks(passwordHost("192.0.2.11"), 1080, models.BindScopeWildcard, ""),
	}}, testExportPassword)

	rec := target.importTunnels(t, file, testExportPassword, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	want := []string{"192.0.2.10 on 1081 wildcard ", "192.0.2.11 on 1080 wildcard "}
	if !reflect.DeepEqual(target.proxies(t), want) {
		t.Fatalf("after the import the proxies are %v, want %v", target.proxies(t), want)
	}
}

// TestImportedSettingsWithAnAPIPortASocksProxyOpensAreNotStored holds the
// import of the settings to the rule a save is held to over a proxy.
func TestImportedSettingsWithAnAPIPortASocksProxyOpensAreNotStored(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	target.registerHost(t, passwordHost("192.0.2.10"))
	target.setSocks(t, "192.0.2.10", 15432, models.BindScopeWildcard, "")

	stored, err := settings.Load(source.db)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}

	stored.APIPort = 15432

	err = settings.Save(source.db, stored)
	if err != nil {
		t.Fatalf("failed to store the settings: %v", err)
	}

	request, err := json.Marshal(importRequest{Password: testExportPassword,
		File: source.exportSettings(t, testExportPassword)})
	if err != nil {
		t.Fatalf("failed to write the import request: %v", err)
	}

	rec := target.call(t, target.handler.ImportSettings, string(request))
	if rec.Code != http.StatusConflict {
		t.Fatalf("the import answered %d, want %d: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}

	answer := readAPIPortTaken(t, rec)
	if answer.Code != string(errImportSettingsAPIPortSocks) {
		t.Errorf("error_code = %q, want %q", answer.Code, errImportSettingsAPIPortSocks)
	}
	if answer.Data.SocksHost == nil || answer.Data.SocksHost.HostIP != "192.0.2.10" ||
		answer.Data.SocksHost.SocksPort != 15432 || answer.Data.SuggestedPort != 15433 {
		t.Errorf("data = %+v, socks_host = %+v, want the proxy on 15432 and 15433 suggested", answer.Data,
			answer.Data.SocksHost)
	}

	after, err := settings.Load(target.db)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}
	if after.APIPort == 15432 {
		t.Errorf("the refused import stored api_port 15432")
	}
}
