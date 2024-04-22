package telemetry

import "net/http"

// noopTelemetry implements a no operations telemetry service
type noopTelemetry struct{}

func defaultNoopTelemetry() Telemetry { return &noopTelemetry{} }

func (n *noopTelemetry) GetOrCreateHistogramMeter(string, []int64) HistogramMeter { return &noopMetric }
func (n *noopTelemetry) GetOrCreateHistogramVecMeter(string, []string, []int64) HistogramVecMeter {
	return &noopMetric
}
func (n *noopTelemetry) GetOrCreateCountMeter(string) CountMeter { return &noopMetric }

func (n *noopTelemetry) GetOrCreateCountVecMeter(string, []string) CountVecMeter {
	return &noopMetric
}

func (n *noopTelemetry) GetOrCreateGaugeMeter(string) GaugeMeter {
	return &noopMetric
}
func (n *noopTelemetry) GetOrCreateGaugeVecMeter(string, []string) GaugeVecMeter {
	return &noopMetric
}

func (n *noopTelemetry) GetOrCreateHandler() http.Handler { return nil }

var noopMetric = noopMeters{}

type noopMeters struct{}

func (n noopMeters) ObserveWithLabels(i int64, m map[string]string) {}

func (n noopMeters) Gauge(int64) {}

func (n noopMeters) GaugeWithLabel(int64, map[string]string) {}

func (n noopMeters) AddWithLabel(int64, map[string]string) {}

func (n noopMeters) Add(int64) {}

func (n noopMeters) Observe(int64) {}

func (n *noopTelemetry) ObserveWithLabels(int64, map[string]string) {}
