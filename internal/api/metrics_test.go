package api

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jollaman999/tunnel-manager/internal/auth"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/tunnel"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// metricsTestVersion is the version the handler under test is handed.
const metricsTestVersion = "9.9.9-test"

// metricsManager is the real manager over the rows with the local forwards and
// the SOCKS5 proxies a test has running laid over it, which are the two things
// the manager holds in memory alone.
type metricsManager struct {
	runningLocalForwards
	socks map[uint]tunnel.SocksState
}

func (m metricsManager) SocksStatuses() map[uint]tunnel.SocksState {
	return m.socks
}

// newMetricsManager builds that manager over db.
func newMetricsManager(t *testing.T, db *gorm.DB, states map[tunnel.LocalForwardKey]tunnel.LocalForwardState,
	socks map[uint]tunnel.SocksState) metricsManager {
	t.Helper()

	base, err := tunnel.NewManager(db, zap.NewNop(), newTestCipher(t), 1)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	return metricsManager{runningLocalForwards: runningLocalForwards{Manager: base, states: states}, socks: socks}
}

// metricsOf runs GetMetrics over db and hands back the body, having checked
// the status and the content type.
func metricsOf(t *testing.T, db *gorm.DB, manager tunnelManager) string {
	t.Helper()

	c, rec := getRequest(t, "/api/metrics", "", "")
	h := NewMetricsHandler(NewHandler(db, manager, zap.NewNop(), newTestCipher(t)), metricsTestVersion)

	err := h.GetMetrics(c)
	if err != nil {
		t.Fatalf("GetMetrics returned error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if got := rec.Header().Get(echo.HeaderContentType); got != metricsContentType {
		t.Fatalf("content type = %q, want %q", got, metricsContentType)
	}

	return rec.Body.String()
}

// parsedSample is one sample line as a scraper reads it, with the label values
// unescaped.
type parsedSample struct {
	name   string
	labels map[string]string
	value  float64
}

// key names the series, the way a scraper tells two series apart.
func (s parsedSample) key() string {
	names := make([]string, 0, len(s.labels))
	for name := range s.labels {
		names = append(names, name)
	}

	sort.Strings(names)

	var b strings.Builder

	b.WriteString(s.name)

	for _, name := range names {
		fmt.Fprintf(&b, ",%s=%q", name, s.labels[name])
	}

	return b.String()
}

var (
	metricNamePattern  = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	metricLabelPattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// parseExposition reads the body the way the text format says it is read, and
// fails the test on anything a strict scraper would refuse: a sample of a
// family with no TYPE before it, a family written in two places, a name or a
// label name the format does not allow, an escape it does not know, a value
// that is not a number, and a series written twice.
func parseExposition(t *testing.T, body string) []parsedSample {
	t.Helper()

	if !strings.HasSuffix(body, "\n") {
		t.Fatalf("the body does not end in a newline: %q", body)
	}

	typed := map[string]bool{}
	helped := map[string]bool{}
	seen := map[string]bool{}
	current := ""

	var samples []parsedSample

	for number, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		where := fmt.Sprintf("line %d %q", number+1, line)

		if strings.HasPrefix(line, "# ") {
			fields := strings.SplitN(line, " ", 4)
			if len(fields) < 4 || !metricNamePattern.MatchString(fields[2]) {
				t.Fatalf("%s: a comment line that is neither a HELP nor a TYPE the format reads", where)
			}

			name := fields[2]

			switch fields[1] {
			case "HELP":
				if helped[name] {
					t.Fatalf("%s: a second HELP for %s", where, name)
				}

				helped[name] = true
			case "TYPE":
				if typed[name] {
					t.Fatalf("%s: a second TYPE for %s", where, name)
				}

				if fields[3] != "gauge" && fields[3] != "counter" {
					t.Fatalf("%s: a type this answer has no reason to use", where)
				}

				typed[name] = true
			default:
				t.Fatalf("%s: a comment line that is neither a HELP nor a TYPE", where)
			}

			if current != name && (seen["family:"+name]) {
				t.Fatalf("%s: %s is written in two places", where, name)
			}

			current = name
			seen["family:"+name] = true

			continue
		}

		sample := parseSampleLine(t, where, line)

		if !typed[sample.name] || current != sample.name {
			t.Fatalf("%s: a sample outside the TYPE of its family", where)
		}

		if seen[sample.key()] {
			t.Fatalf("%s: the series %s is written twice", where, sample.key())
		}

		seen[sample.key()] = true
		samples = append(samples, sample)
	}

	return samples
}

// parseSampleLine reads one sample line: the name, the labels between braces
// with their values unescaped, one space and the value.
func parseSampleLine(t *testing.T, where, line string) parsedSample {
	t.Helper()

	sample := parsedSample{labels: map[string]string{}}

	end := strings.IndexAny(line, "{ ")
	if end < 0 {
		t.Fatalf("%s: a sample with no value", where)
	}

	sample.name = line[:end]
	if !metricNamePattern.MatchString(sample.name) {
		t.Fatalf("%s: %q is not a metric name", where, sample.name)
	}

	rest := line[end:]

	if strings.HasPrefix(rest, "{") {
		rest = rest[1:]

		for !strings.HasPrefix(rest, "}") {
			eq := strings.Index(rest, `="`)
			if eq < 0 {
				t.Fatalf("%s: a label with no quoted value", where)
			}

			name := rest[:eq]
			if !metricLabelPattern.MatchString(name) {
				t.Fatalf("%s: %q is not a label name", where, name)
			}

			if _, twice := sample.labels[name]; twice {
				t.Fatalf("%s: the label %s is written twice", where, name)
			}

			rest = rest[eq+2:]

			var value strings.Builder

			for {
				if rest == "" {
					t.Fatalf("%s: a label value that is never closed", where)
				}

				r := rest[0]
				rest = rest[1:]

				if r == '"' {
					break
				}

				if r != '\\' {
					value.WriteByte(r)

					continue
				}

				if rest == "" {
					t.Fatalf("%s: a backslash at the end of the line", where)
				}

				switch rest[0] {
				case '\\':
					value.WriteByte('\\')
				case '"':
					value.WriteByte('"')
				case 'n':
					value.WriteByte('\n')
				default:
					t.Fatalf("%s: the escape \\%c, which the format does not have", where, rest[0])
				}

				rest = rest[1:]
			}

			sample.labels[name] = value.String()

			if strings.HasPrefix(rest, ",") {
				rest = rest[1:]
			} else if !strings.HasPrefix(rest, "}") {
				t.Fatalf("%s: labels not separated by a comma", where)
			}
		}

		rest = rest[1:]
	}

	if !strings.HasPrefix(rest, " ") || strings.Contains(rest[1:], " ") {
		t.Fatalf("%s: the value is not one field after one space", where)
	}

	value, err := strconv.ParseFloat(rest[1:], 64)
	if err != nil {
		t.Fatalf("%s: the value is not a number: %v", where, err)
	}

	sample.value = value

	return sample
}

// findSample is the one sample of the name whose labels include want, or
// fails.
func findSample(t *testing.T, samples []parsedSample, name string, want map[string]string) parsedSample {
	t.Helper()

	var found []parsedSample

	for _, sample := range samples {
		if sample.name != name {
			continue
		}

		matches := true
		for label, value := range want {
			if sample.labels[label] != value {
				matches = false

				break
			}
		}

		if matches {
			found = append(found, sample)
		}
	}

	if len(found) != 1 {
		t.Fatalf("%d samples of %s carry %v, want 1: %v", len(found), name, want, found)
	}

	return found[0]
}

// countSamples is how many samples of the name there are.
func countSamples(samples []parsedSample, name string) int {
	n := 0

	for _, sample := range samples {
		if sample.name == name {
			n++
		}
	}

	return n
}

// metricsFixtureState is the state the tests below are run over: tunnels in
// three statuses over two Hosts, local forwards running in three and switched
// off in one, and a SOCKS5 proxy.
func metricsFixtureState(t *testing.T) (*gorm.DB, metricsManager) {
	t.Helper()

	hosts := []models.Host{statusHost(1, true), statusHost(2, true)}
	hosts[1].SocksEnabled = true
	hosts[1].SocksPort = 1080

	sps := []models.ServicePort{statusServicePort(1), statusServicePort(2)}
	tunnels := []models.Tunnel{
		{HostID: 1, SPID: 1, Status: "connected", Local: "127.0.0.1:18081", Remote: "198.51.100.10:8081"},
		{HostID: 1, SPID: 2, Status: "error", RetryCount: 4, Local: "127.0.0.1:18082", Remote: "198.51.100.10:8082"},
		{HostID: 2, SPID: 1, Status: "connected", Local: "127.0.0.1:18081", Remote: "198.51.100.10:8081"},
		{HostID: 2, SPID: 2, Status: "reconnecting", RetryCount: 2, Local: "127.0.0.1:18082", Remote: "198.51.100.10:8082"},
	}

	db := newRowsDB(t, hosts, sps, tunnels)
	storeLocalForwards(t, db, []models.LocalForward{
		statusLocalForward(1, 1, 19000, models.BindScopeWildcard, true),
		statusLocalForward(2, 1, 19001, models.BindScopeWildcard, false),
		statusLocalForward(3, 2, 19002, models.BindScopeWildcard, true),
		statusLocalForward(4, 2, 19003, models.BindScopeWildcard, true),
	})

	states := map[tunnel.LocalForwardKey]tunnel.LocalForwardState{
		{HostID: 1, Number: 1}: {Status: "connected"},
		{HostID: 2, Number: 3}: {Status: "error", RetryCount: 3},
		{HostID: 2, Number: 4}: {Status: "starting"},
	}
	socks := map[uint]tunnel.SocksState{
		2: {Status: "reconnecting", RetryCount: 1},
	}

	return db, newMetricsManager(t, db, states, socks)
}

// TestMetricsParseAsThePrometheusTextFormat reads the whole answer with the
// strict reader above and checks what each family says about the state.
func TestMetricsParseAsThePrometheusTextFormat(t *testing.T) {
	db, manager := metricsFixtureState(t)

	samples := parseExposition(t, metricsOf(t, db, manager))

	info := findSample(t, samples, "tunnel_manager_info", nil)
	if info.labels["version"] != metricsTestVersion || info.value != 1 {
		t.Errorf("tunnel_manager_info = %v, want version %q and 1", info, metricsTestVersion)
	}

	forwards := map[[2]string]float64{
		{"service_port", "connected"}:     2,
		{"service_port", "reconnecting"}:  1,
		{"service_port", "error"}:         1,
		{"local_forward", "connected"}:    1,
		{"local_forward", "reconnecting"}: 0,
		{"local_forward", "error"}:        1,
		{"local_forward", "starting"}:     1,
		{"socks", "connected"}:            0,
		{"socks", "reconnecting"}:         1,
		{"socks", "error"}:                0,
	}
	for key, want := range forwards {
		got := findSample(t, samples, "tunnel_manager_forwards", map[string]string{"kind": key[0], "status": key[1]})
		if got.value != want {
			t.Errorf("tunnel_manager_forwards{kind=%q,status=%q} = %v, want %v", key[0], key[1], got.value, want)
		}
	}

	if n := countSamples(samples, "tunnel_manager_forwards"); n != len(forwards) {
		t.Errorf("%d samples of tunnel_manager_forwards, want %d", n, len(forwards))
	}

	// One series per running forward: four tunnels, three local forwards the
	// manager holds and one proxy. The forward that is switched off is in
	// none of them.
	for _, name := range []string{"tunnel_manager_forward_up", "tunnel_manager_forward_retries"} {
		if n := countSamples(samples, name); n != 8 {
			t.Errorf("%d samples of %s, want 8", n, name)
		}
	}

	up := findSample(t, samples, "tunnel_manager_forward_up",
		map[string]string{"kind": "service_port", "host": "192.0.2.1", "local_port": "18081"})
	if up.value != 1 || up.labels["remote"] != "198.51.100.10:8081" {
		t.Errorf("the connected tunnel reads %v, want 1 with its remote", up)
	}

	up = findSample(t, samples, "tunnel_manager_forward_up",
		map[string]string{"kind": "local_forward", "host": "192.0.2.2", "local_port": "19002"})
	if up.value != 0 || up.labels["remote"] != "198.51.100.20:3306" {
		t.Errorf("the local forward in error reads %v, want 0 with its target", up)
	}

	up = findSample(t, samples, "tunnel_manager_forward_up",
		map[string]string{"kind": "socks", "host": "192.0.2.2", "local_port": "1080"})
	if _, carried := up.labels["remote"]; carried || up.value != 0 {
		t.Errorf("the proxy reads %v, want 0 and no remote", up)
	}

	retries := findSample(t, samples, "tunnel_manager_forward_retries",
		map[string]string{"kind": "service_port", "host": "192.0.2.1", "local_port": "18082"})
	if retries.value != 4 {
		t.Errorf("the retries of the tunnel in error read %v, want 4", retries.value)
	}

	retries = findSample(t, samples, "tunnel_manager_forward_retries",
		map[string]string{"kind": "local_forward", "local_port": "19002"})
	if retries.value != 3 {
		t.Errorf("the retries of the local forward in error read %v, want 3", retries.value)
	}

	desired := findSample(t, samples, "tunnel_manager_forwards_desired", map[string]string{"kind": "service_port"})
	if desired.value != 4 {
		t.Errorf("desired service port tunnels = %v, want 4", desired.value)
	}

	desired = findSample(t, samples, "tunnel_manager_forwards_desired", map[string]string{"kind": "local_forward"})
	if desired.value != 3 {
		t.Errorf("desired local forwards = %v, want 3", desired.value)
	}

	if n := countSamples(samples, "tunnel_manager_host_info"); n != 2 {
		t.Errorf("%d samples of tunnel_manager_host_info, want one per Host", n)
	}
}

// TestMetricsCountWhatTheStatusAnswerCounts holds the counts of the two to the
// same state: the three statuses of the status answer are the metrics of the
// two sorts it counts, added, and so are desired_tunnels and the Hosts waiting
// for a key.
func TestMetricsCountWhatTheStatusAnswerCounts(t *testing.T) {
	db, manager := metricsFixtureState(t)

	err := db.Model(&models.Host{}).Where("id = ?", 1).Update("pending_host_key", "ssh-ed25519 AAAA").Error
	if err != nil {
		t.Fatalf("failed to leave a key waiting: %v", err)
	}

	samples := parseExposition(t, metricsOf(t, db, manager))

	c, rec := getRequest(t, "/api/status", "", "")

	err = NewHandler(db, manager, zap.NewNop(), newTestCipher(t)).GetStatus(c)
	if err != nil || rec.Code != http.StatusOK {
		t.Fatalf("GetStatus answered %d %v: %s", rec.Code, err, rec.Body.String())
	}

	_, data := decodeResponse(t, rec)

	sum := func(name string, labels map[string]string, kinds ...string) int {
		total := 0.0

		for _, kind := range kinds {
			want := map[string]string{"kind": kind}
			for label, value := range labels {
				want[label] = value
			}

			total += findSample(t, samples, name, want).value
		}

		return int(total)
	}

	sorts := []string{statusKindServicePort, statusKindLocalForward}
	pairs := map[string]int{
		"connected_tunnels":    sum("tunnel_manager_forwards", map[string]string{"status": "connected"}, sorts...),
		"reconnecting_tunnels": sum("tunnel_manager_forwards", map[string]string{"status": "reconnecting"}, sorts...),
		"error_tunnels":        sum("tunnel_manager_forwards", map[string]string{"status": "error"}, sorts...),
		"desired_tunnels":      sum("tunnel_manager_forwards_desired", nil, sorts...),
		"host_keys_unapproved": int(findSample(t, samples, "tunnel_manager_host_keys_waiting",
			map[string]string{"reason": "unapproved"}).value),
		"host_keys_mismatched": int(findSample(t, samples, "tunnel_manager_host_keys_waiting",
			map[string]string{"reason": "mismatched"}).value),
	}

	for key, fromMetrics := range pairs {
		if fromStatus := statusCount(t, data, key); fromStatus != fromMetrics {
			t.Errorf("%s: the status answer says %d and the metrics %d", key, fromStatus, fromMetrics)
		}
	}

	if pairs["host_keys_unapproved"] != 1 {
		t.Errorf("host_keys_unapproved = %d, want the one Host left waiting", pairs["host_keys_unapproved"])
	}
}

// TestMetricsEscapeTheLabelValues stores a Host whose address and description
// carry every character the text format escapes, and reads them back through
// the strict reader: what comes out has to be what went in.
func TestMetricsEscapeTheLabelValues(t *testing.T) {
	address := `edge"1\.example.com`
	description := "rack \"A\" \\ row 3\nsecond line"

	host := models.Host{ID: 1, Address: address, Port: 22, User: "root", Description: description, Enabled: true}
	db := newRowsDB(t, []models.Host{host}, []models.ServicePort{statusServicePort(1)}, []models.Tunnel{
		{HostID: 1, SPID: 1, Status: "connected", Local: "127.0.0.1:18081", Remote: "198.51.100.10:8081"},
	})

	body := metricsOf(t, db, newMetricsManager(t, db, nil, nil))

	if !strings.Contains(body, `description="rack \"A\" \\ row 3\nsecond line"`) {
		t.Errorf("the description is not written escaped:\n%s", body)
	}

	samples := parseExposition(t, body)

	info := findSample(t, samples, "tunnel_manager_host_info", nil)
	if info.labels["host"] != address || info.labels["description"] != description {
		t.Errorf("the Host reads back as %q %q, want %q %q",
			info.labels["host"], info.labels["description"], address, description)
	}

	up := findSample(t, samples, "tunnel_manager_forward_up", nil)
	if up.labels["host"] != address {
		t.Errorf("the tunnel names its Host %q, want %q", up.labels["host"], address)
	}
}

// metricsAuthServer is the server wired the way main wires it, with the
// session middleware in front of the metrics and the token routes, over the
// fixture state.
func metricsAuthServer(t *testing.T) *tokenFixture {
	t.Helper()

	db, manager := metricsFixtureState(t)

	err := db.AutoMigrate(&models.User{}, &models.APIToken{})
	if err != nil {
		t.Fatalf("failed to migrate the database: %v", err)
	}

	hash, err := auth.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("failed to hash the password: %v", err)
	}

	err = db.Create(&models.User{Username: testUsername, PasswordHash: hash}).Error
	if err != nil {
		t.Fatalf("failed to write the account: %v", err)
	}

	e := echo.New()
	authHandler := NewAuthHandler(db, zap.NewNop(), "")
	metrics := NewMetricsHandler(NewHandler(db, manager, zap.NewNop(), newTestCipher(t)), metricsTestVersion)

	g := e.Group("/api")
	g.Use(authHandler.RequireSession())
	g.POST("/login", authHandler.Login)
	g.POST("/token", authHandler.CreateToken)
	g.GET("/metrics", metrics.GetMetrics)

	return &tokenFixture{e: e, db: db}
}

// TestMetricsAreReachedByAReadToken covers who reaches the route: a token with
// the read scope and a session do, a token without it is refused, and nothing
// at all is asked to authenticate.
func TestMetricsAreReachedByAReadToken(t *testing.T) {
	f := metricsAuthServer(t)
	cookies := f.signIn(t)
	reader := f.create(t, cookies, `{"name":"prometheus","scopes":["read"]}`)
	writer := f.create(t, cookies, `{"name":"hosts","scopes":["hosts"]}`)

	rec := f.withBearer(http.MethodGet, "/api/metrics", "", reader.Token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("a read token answered %d, want 200: %s", rec.Code, rec.Body.String())
	}

	if got := rec.Header().Get(echo.HeaderContentType); got != metricsContentType {
		t.Errorf("content type = %q, want %q", got, metricsContentType)
	}

	parseExposition(t, rec.Body.String())

	rec = f.withBearer(http.MethodGet, "/api/metrics", "", writer.Token, "")
	if rec.Code != http.StatusForbidden || decodeTokenAnswer(t, rec).ErrorCode != string(errAuthTokenScopeMissing) {
		t.Errorf("a token without read answered %d %s, want 403 %s",
			rec.Code, rec.Body.String(), errAuthTokenScopeMissing)
	}

	rec = do(f.e, http.MethodGet, "/api/metrics", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no credential answered %d, want 401", rec.Code)
	}

	rec = do(f.e, http.MethodGet, "/api/metrics", "", cookies...)
	if rec.Code != http.StatusOK {
		t.Errorf("a session answered %d, want 200", rec.Code)
	}
}
