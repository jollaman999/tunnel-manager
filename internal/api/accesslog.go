package api

import (
	"encoding/json"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
)

// accessLogLine is one request in the access log. The fields, their names and
// their order are the ones the access log carried before, so a line written
// now reads the way the lines already in a journal do.
type accessLogLine struct {
	Time         string `json:"time"`
	ID           string `json:"id"`
	RemoteIP     string `json:"remote_ip"`
	Host         string `json:"host"`
	Method       string `json:"method"`
	URI          string `json:"uri"`
	UserAgent    string `json:"user_agent"`
	Status       int    `json:"status"`
	Error        string `json:"error"`
	Latency      int64  `json:"latency"`
	LatencyHuman string `json:"latency_human"`
	BytesIn      int64  `json:"bytes_in"`
	BytesOut     int64  `json:"bytes_out"`
}

// AccessLog writes a JSON line to out for every request served, after the
// answer has gone out. remote_ip is what the IPExtractor of the server makes
// of the request (ClientAddress), so the access log and every other place that
// names the client agree on the address.
//
// A failed handler is answered here, through the error handler of the server,
// and not after this returns, so that the status and the size written down are
// the ones the client received.
func AccessLog(out io.Writer) echo.MiddlewareFunc {
	var mu sync.Mutex

	return middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		HandleError:      true,
		LogLatency:       true,
		LogRemoteIP:      true,
		LogHost:          true,
		LogMethod:        true,
		LogURI:           true,
		LogRequestID:     true,
		LogUserAgent:     true,
		LogStatus:        true,
		LogContentLength: true,
		LogResponseSize:  true,
		LogValuesFunc: func(c *echo.Context, v middleware.RequestLoggerValues) error {
			line := accessLogLine{
				Time:         v.StartTime.Add(v.Latency).Format(time.RFC3339Nano),
				ID:           v.RequestID,
				RemoteIP:     v.RemoteIP,
				Host:         v.Host,
				Method:       v.Method,
				URI:          v.URI,
				UserAgent:    v.UserAgent,
				Status:       v.Status,
				Latency:      int64(v.Latency),
				LatencyHuman: v.Latency.String(),
				BytesOut:     max(v.ResponseSize, 0),
			}
			if v.Error != nil {
				line.Error = v.Error.Error()
			}
			// Content-Length is what the client said it sends. One that is
			// not a number is written as nothing having been sent.
			line.BytesIn, _ = strconv.ParseInt(v.ContentLength, 10, 64)

			mu.Lock()
			defer mu.Unlock()

			encoder := json.NewEncoder(out)
			encoder.SetEscapeHTML(false)

			return encoder.Encode(line)
		},
	})
}
