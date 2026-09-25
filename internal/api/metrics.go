package api

import (
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/tunnel"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

// metricsContentType is what the answer is sent as: the text format of
// Prometheus, version 0.0.4, which is the one every scraper that reads
// Prometheus reads, telegraf among them.
const metricsContentType = "text/plain; version=0.0.4; charset=utf-8"

// statusKindSocks is the third sort of forward the metrics count. The status
// table carries only the other two, because a SOCKS5 proxy is shown on the Host
// it belongs to rather than as a row of its own, but a scraper that is asked
// whether everything is up has to be told about it too.
const statusKindSocks = "socks"

// MetricsHandler answers GET /metrics.
//
// It holds the Handler that serves the status rather than a database handle and
// a manager of its own, for the reason TransferHandler does: the desired counts
// are asked of the same manager GetStatus asks, and a second copy of that wiring
// here would be a second place that could drift from what the screen shows.
//
// version is the version of this binary, handed in from the startup that knows
// it, and goes out as tunnel_manager_info.
type MetricsHandler struct {
	status  *Handler
	version string
}

func NewMetricsHandler(status *Handler, version string) *MetricsHandler {
	return &MetricsHandler{
		status:  status,
		version: version,
	}
}

// metricLabel is one label of a sample, in the order it is written.
type metricLabel struct {
	name  string
	value string
}

// metricSample is one line of a family: its labels and its value.
type metricSample struct {
	labels []metricLabel
	value  float64
}

// metricFamily is one metric as the text format groups it: the HELP and TYPE
// lines and then every sample of it.
type metricFamily struct {
	name    string
	help    string
	kind    string
	samples []metricSample
}

// metricHelpEscaper and metricLabelEscaper escape what the text format says
// must be escaped. A HELP line may not carry a raw newline and a backslash in
// it would be read as the start of an escape. A label value is quoted, so a
// double quote has to be escaped as well.
var (
	metricHelpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	metricLabelEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)
)

// writeMetricFamilies writes the families in the order given, each sample in
// the order it was added.
func writeMetricFamilies(b *strings.Builder, families []metricFamily) {
	for _, family := range families {
		b.WriteString("# HELP ")
		b.WriteString(family.name)
		b.WriteByte(' ')
		b.WriteString(metricHelpEscaper.Replace(family.help))
		b.WriteByte('\n')

		b.WriteString("# TYPE ")
		b.WriteString(family.name)
		b.WriteByte(' ')
		b.WriteString(family.kind)
		b.WriteByte('\n')

		for _, sample := range family.samples {
			b.WriteString(family.name)

			if len(sample.labels) > 0 {
				b.WriteByte('{')

				for i, label := range sample.labels {
					if i > 0 {
						b.WriteByte(',')
					}

					b.WriteString(label.name)
					b.WriteString(`="`)
					b.WriteString(metricLabelEscaper.Replace(label.value))
					b.WriteByte('"')
				}

				b.WriteByte('}')
			}

			b.WriteByte(' ')
			b.WriteString(strconv.FormatFloat(sample.value, 'g', -1, 64))
			b.WriteByte('\n')
		}
	}
}

// metricForward is one running forward as the metrics name it.
//
// kind, host and local_port are what tells one series from another, and every
// one of them is unique over its sort: the Host address is unique over the
// Hosts, the local port of a service port is unique over the service ports, the
// local port of a local forward is unique over the local forwards, and a Host
// runs one SOCKS5 proxy. So there is one series per forward and never two,
// which a scraper would refuse the whole answer for.
type metricForward struct {
	kind       string
	host       string
	localPort  string
	remote     string
	status     string
	retryCount int
}

// labels is the labels the series of this forward carry. remote is left off
// where the forward reaches no one address, which is the SOCKS5 proxy: it
// reaches wherever each client asks it to.
func (f metricForward) labels(withRemote bool) []metricLabel {
	labels := []metricLabel{
		{name: "kind", value: f.kind},
		{name: "host", value: f.host},
		{name: "local_port", value: f.localPort},
	}

	if withRemote && f.kind != statusKindSocks {
		labels = append(labels, metricLabel{name: "remote", value: f.remote})
	}

	return labels
}

// portOf is the port of an address written as host:port, or the address as
// it is when it is not written that way.
func portOf(address string) string {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}

	return port
}

// GetMetrics answers with the state of the installation in the text format of
// Prometheus.
//
// Everything in it is read from what the status answer is read from, and
// nothing is counted in the data path for it: the tunnels from their rows, the
// local forwards and the SOCKS5 proxies from what the manager holds of the
// running ones, and what should be running from the manager, which keeps it
// from the last reconcile pass. The counts of the three statuses of the status
// answer are therefore the sum of this answer over the two sorts that answer
// counts, for the same state.
//
// A forward is in the per-forward series while it runs, and not otherwise: a
// tunnel row is removed when its tunnel is stopped, and a local forward or a
// proxy that is switched off reports nothing. A forward that is off is not a
// forward that is down, and a series at 0 for it would be an alert for nothing.
//
// @Summary      The state of the installation in the Prometheus text format
// @Description  Answers in the Prometheus text exposition format, version 0.0.4, rather than in the JSON every other route answers with, so that Prometheus, telegraf and anything else that scrapes that format can read it as it is. A token needs the read scope.
// @Description  tunnel_manager_forwards counts the running forwards by kind and status; its connected, reconnecting and error series over service_port and local_forward add up to the counts GET /status answers with. tunnel_manager_forwards_desired is desired_tunnels of GET /status, by kind. tunnel_manager_forward_up and tunnel_manager_forward_retries have one series per running forward. tunnel_manager_host_info carries the description of each Host, to be joined on host.
// @Tags         status
// @Produce  plain
// @Success  200  {string}  string  "The metrics"
// @Failure  401  {object}  api.errorBody  "Authentication required"
// @Failure  403  {object}  api.errorBody  "The token does not carry the read scope"
// @Failure  500  {object}  api.errorBody  "A count or a row could not be read"
// @Router       /metrics [get]
func (m *MetricsHandler) GetMetrics(c echo.Context) error {
	h := m.status

	desiredTunnels, err := h.manager.DesiredTunnelCount()
	if err != nil {
		h.logger.Error("failed to count the tunnels that should be running",
			logid.StatusTunnelsToRunCountFailed.Field(),
			zap.Error(err))
		return failure(c, http.StatusInternalServerError, errStatusDesiredCountFailed)
	}

	desiredLocalForwards, err := h.manager.DesiredLocalForwardCount()
	if err != nil {
		h.logger.Error("failed to count the local forwards that should be running",
			logid.LocalForwardCountFailed.Field(),
			zap.Error(err))
		return failure(c, http.StatusInternalServerError, errStatusDesiredCountFailed)
	}

	var tunnels []models.Tunnel

	err = h.db.Order("host_id, sp_id").Find(&tunnels).Error
	if err != nil {
		h.logger.Error("failed to fetch the tunnel status", logid.StatusTunnelsFetchFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errStatusFetchFailed)
	}

	var forwards []models.LocalForward

	err = h.db.Order("host_id, number").Find(&forwards).Error
	if err != nil {
		h.logger.Error("failed to fetch local forwards", logid.LocalForwardListFetchFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errStatusFetchFailed)
	}

	var hosts []models.Host

	err = h.db.Select("id", "address", "port", "description", "socks_port").Order("id").Find(&hosts).Error
	if err != nil {
		h.logger.Error("failed to fetch Host", logid.HostFetchFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errStatusFetchFailed)
	}

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

	states := h.manager.LocalForwardStatuses()
	socks := h.manager.SocksStatuses()

	hostByID := make(map[uint]*models.Host, len(hosts))
	for i := range hosts {
		hostByID[hosts[i].ID] = &hosts[i]
	}

	running := make([]metricForward, 0, len(tunnels)+len(states)+len(socks))

	for _, t := range tunnels {
		host := ""
		if owner, found := hostByID[t.HostID]; found {
			host = owner.Address
		} else if address, _, err := net.SplitHostPort(t.Server); err == nil {
			host = address
		}

		running = append(running, metricForward{
			kind:       statusKindServicePort,
			host:       host,
			localPort:  portOf(t.Local),
			remote:     t.Remote,
			status:     t.Status,
			retryCount: t.RetryCount,
		})
	}

	for _, lf := range forwards {
		state, found := states[tunnel.LocalForwardKey{HostID: lf.HostID, Number: lf.Number}]
		if !found {
			continue
		}

		owner := models.Host{}
		if host, known := hostByID[lf.HostID]; known {
			owner = *host
		}

		_, _, _, target := tunnel.LocalForwardAddresses(&owner, &lf)

		running = append(running, metricForward{
			kind:       statusKindLocalForward,
			host:       owner.Address,
			localPort:  strconv.Itoa(lf.LocalPort),
			remote:     target,
			status:     state.Status,
			retryCount: state.RetryCount,
		})
	}

	socksHosts := make([]uint, 0, len(socks))
	for hostID := range socks {
		socksHosts = append(socksHosts, hostID)
	}

	sort.Slice(socksHosts, func(i, j int) bool { return socksHosts[i] < socksHosts[j] })

	for _, hostID := range socksHosts {
		owner, found := hostByID[hostID]
		if !found {
			continue
		}

		state := socks[hostID]

		running = append(running, metricForward{
			kind:       statusKindSocks,
			host:       owner.Address,
			localPort:  strconv.Itoa(owner.SocksPort),
			status:     state.Status,
			retryCount: state.RetryCount,
		})
	}

	// The counts are taken over what the status answer counts: the tunnel
	// rows, and what the manager holds of the running local forwards and
	// proxies. A local forward the manager holds and whose row is gone is
	// counted here as GetStatus counts it, though it has no series of its own
	// below, because there is nothing left to name it by.
	counts := map[string]map[string]int{
		statusKindServicePort:  {},
		statusKindLocalForward: {},
		statusKindSocks:        {},
	}

	for _, t := range tunnels {
		counts[statusKindServicePort][t.Status]++
	}

	for _, state := range states {
		counts[statusKindLocalForward][state.Status]++
	}

	for _, state := range socks {
		counts[statusKindSocks][state.Status]++
	}

	families := []metricFamily{
		{
			name: "tunnel_manager_info",
			help: "The version of this Tunnel Manager. Always 1.",
			kind: "gauge",
			samples: []metricSample{
				{labels: []metricLabel{{name: "version", value: m.version}}, value: 1},
			},
		},
		{
			name:    "tunnel_manager_forwards",
			help:    "How many running forwards are in each status, by kind.",
			kind:    "gauge",
			samples: metricCountSamples(counts),
		},
		{
			name: "tunnel_manager_forwards_desired",
			help: "How many forwards should be running as of the last reconcile pass, by kind.",
			kind: "gauge",
			samples: []metricSample{
				{labels: []metricLabel{{name: "kind", value: statusKindServicePort}}, value: float64(desiredTunnels)},
				{labels: []metricLabel{{name: "kind", value: statusKindLocalForward}}, value: float64(desiredLocalForwards)},
			},
		},
		{
			name:    "tunnel_manager_forward_up",
			help:    "Whether a running forward is connected: 1 if it is, 0 if it is starting, reconnecting, in error or held at a host key.",
			kind:    "gauge",
			samples: make([]metricSample, 0, len(running)),
		},
		{
			name:    "tunnel_manager_forward_retries",
			help:    "How many times a running forward has retried since it last connected.",
			kind:    "gauge",
			samples: make([]metricSample, 0, len(running)),
		},
		{
			name: "tunnel_manager_host_keys_waiting",
			help: "How many Hosts have a host key waiting to be approved: unapproved is a Host never approved, mismatched one trusted on another key.",
			kind: "gauge",
			samples: []metricSample{
				{labels: []metricLabel{{name: "reason", value: "unapproved"}}, value: float64(hostKeysUnapproved)},
				{labels: []metricLabel{{name: "reason", value: "mismatched"}}, value: float64(hostKeysMismatched)},
			},
		},
		{
			name:    "tunnel_manager_host_info",
			help:    "The description of each Host, to be joined to the forward series on host. Always 1.",
			kind:    "gauge",
			samples: make([]metricSample, 0, len(hosts)),
		},
	}

	up := &families[3]
	retries := &families[4]

	for _, forward := range running {
		value := 0.0
		if forward.status == statusConnected {
			value = 1
		}

		up.samples = append(up.samples, metricSample{labels: forward.labels(true), value: value})
		retries.samples = append(retries.samples, metricSample{
			labels: forward.labels(false),
			value:  float64(forward.retryCount),
		})
	}

	hostInfo := &families[6]

	for _, host := range hosts {
		hostInfo.samples = append(hostInfo.samples, metricSample{
			labels: []metricLabel{
				{name: "host", value: host.Address},
				{name: "description", value: host.Description},
			},
			value: 1,
		})
	}

	var b strings.Builder

	writeMetricFamilies(&b, families)

	return c.Blob(http.StatusOK, metricsContentType, []byte(b.String()))
}

// metricCountSamples turns the counts into samples, one per kind and status.
//
// The three statuses the status answer counts are written for every kind even
// at zero, so that a series does not vanish while nothing is in it: an alert on
// a count of forwards in error has to be able to read a zero. Any other status
// is written only while something is in it, and the statuses are written in
// the order of their names so the answer is the same from one scrape to the
// next.
func metricCountSamples(counts map[string]map[string]int) []metricSample {
	var samples []metricSample

	for _, kind := range []string{statusKindServicePort, statusKindLocalForward, statusKindSocks} {
		byStatus := counts[kind]

		for _, status := range []string{statusConnected, statusReconnecting, statusError} {
			if _, found := byStatus[status]; !found {
				byStatus[status] = 0
			}
		}

		statuses := make([]string, 0, len(byStatus))
		for status := range byStatus {
			statuses = append(statuses, status)
		}

		sort.Strings(statuses)

		for _, status := range statuses {
			samples = append(samples, metricSample{
				labels: []metricLabel{
					{name: "kind", value: kind},
					{name: "status", value: status},
				},
				value: float64(byStatus[status]),
			})
		}
	}

	return samples
}
