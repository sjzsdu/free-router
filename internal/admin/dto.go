package admin

import (
	"time"

	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/credentials"
	"github.com/sjzsdu/free-router/internal/health"
	"github.com/sjzsdu/free-router/internal/routing"
)

type ModelEligibility struct {
	Model      string `json:"model"`
	Capability string `json:"capability"`
	Eligible   bool   `json:"eligible"`
	Reason     string `json:"reason,omitempty"`
}

type RuntimeState struct {
	Status         string    `json:"status"`
	PID            int       `json:"pid"`
	Version        string    `json:"version"`
	StartedAt      time.Time `json:"started_at"`
	UptimeSeconds  int64     `json:"uptime_seconds"`
	ServiceManager string    `json:"service_manager"`
	Models         int       `json:"models"`
	Requests       uint64    `json:"requests"`
	Failed         int       `json:"failed"`
}

type StateResponse struct {
	Config         routing.Config      `json:"config"`
	ConfigPath     string              `json:"config_path"`
	Models         []catalog.Model     `json:"models"`
	Catalog        catalog.Status      `json:"catalog"`
	Providers      []map[string]any    `json:"providers"`
	Credentials    []credentials.Entry `json:"credentials"`
	Health         []health.State      `json:"health"`
	ProviderHealth []health.State      `json:"provider_health"`
	Summary        health.Summary      `json:"summary"`
	HealthProbe    ProbeStatus         `json:"health_probe"`
	Runtime        RuntimeState        `json:"runtime"`
	Eligibility    []ModelEligibility  `json:"eligibility"`
}
