package api

import (
	"testing"

	"github.com/jollaman999/tunnel-manager/internal/models"
)

func boolPtr(b bool) *bool {
	return &b
}

func intPtr(i int) *int {
	return &i
}

func TestResolveTunnelActions(t *testing.T) {
	baseHost := func(enabled bool) models.Host {
		return models.Host{
			ID:          1,
			IP:          "10.0.0.1",
			Port:        22,
			User:        "root",
			Password:    "pass",
			Description: "old",
			Enabled:     enabled,
		}
	}

	tests := []struct {
		name         string
		host         models.Host
		req          models.UpdateHostRequest
		finalEnabled bool
		needStop     bool
		needStart    bool
	}{
		{
			name:         "enabled host disabled by request",
			host:         baseHost(true),
			req:          models.UpdateHostRequest{Enabled: boolPtr(false)},
			finalEnabled: false,
			needStop:     true,
			needStart:    false,
		},
		{
			name:         "disabled host enabled by request",
			host:         baseHost(false),
			req:          models.UpdateHostRequest{Enabled: boolPtr(true)},
			finalEnabled: true,
			needStop:     false,
			needStart:    true,
		},
		{
			name:         "enabled host with changed connection info",
			host:         baseHost(true),
			req:          models.UpdateHostRequest{IP: "10.0.0.2", Port: intPtr(2222), User: "admin", Password: "new"},
			finalEnabled: true,
			needStop:     true,
			needStart:    true,
		},
		{
			name:         "disabled host with changed connection info only",
			host:         baseHost(false),
			req:          models.UpdateHostRequest{IP: "10.0.0.2", Port: intPtr(2222), User: "admin", Password: "new"},
			finalEnabled: false,
			needStop:     false,
			needStart:    false,
		},
		{
			name:         "disabled host with changed description only",
			host:         baseHost(false),
			req:          models.UpdateHostRequest{Description: "new"},
			finalEnabled: false,
			needStop:     false,
			needStart:    false,
		},
		{
			name:         "enabled host with changed description only",
			host:         baseHost(true),
			req:          models.UpdateHostRequest{Description: "new"},
			finalEnabled: true,
			needStop:     false,
			needStart:    false,
		},
		{
			name:         "enabled host with same connection info",
			host:         baseHost(true),
			req:          models.UpdateHostRequest{IP: "10.0.0.1", Port: intPtr(22), User: "root", Password: "pass"},
			finalEnabled: true,
			needStop:     false,
			needStart:    false,
		},
		{
			name:         "disabled host enabled with changed connection info",
			host:         baseHost(false),
			req:          models.UpdateHostRequest{IP: "10.0.0.2", Enabled: boolPtr(true)},
			finalEnabled: true,
			needStop:     false,
			needStart:    true,
		},
		{
			name:         "enabled host disabled with changed connection info",
			host:         baseHost(true),
			req:          models.UpdateHostRequest{IP: "10.0.0.2", Enabled: boolPtr(false)},
			finalEnabled: false,
			needStop:     true,
			needStart:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host := tt.host
			req := tt.req

			finalEnabled, needStop, needStart := resolveTunnelActions(&req, &host)
			if finalEnabled != tt.finalEnabled {
				t.Errorf("finalEnabled = %v, want %v", finalEnabled, tt.finalEnabled)
			}
			if needStop != tt.needStop {
				t.Errorf("needStop = %v, want %v", needStop, tt.needStop)
			}
			if needStart != tt.needStart {
				t.Errorf("needStart = %v, want %v", needStart, tt.needStart)
			}
		})
	}
}
