package monitor

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"carillon/internal/control"
	"carillon/internal/discipline"
	ntpserver "carillon/internal/server"
)

// TestServerResultsPartitionReceivedTraffic pins the property a traffic chart
// depends on: summing carillon_server_requests_total over the result label
// gives every datagram the listener read, with nothing counted twice.
func TestServerResultsPartitionReceivedTraffic(t *testing.T) {
	c := control.CounterStats{
		Served: 10, Unsynced: 3, KoD: 2,
		Denied: 1, Martian: 2, RateLimited: 4, BadAuth: 1,
		BadVersion: 1, NonClient: 5, Malformed: 2, Oversize: 1,
	}
	var sum uint64
	for _, v := range serverResults(&c) {
		sum += v
	}
	if sum != c.Requests() || sum != 27 {
		t.Fatalf("result labels sum to %d, received %d", sum, c.Requests())
	}
}

// gatherMetrics renders the collector's output as one comparable line per
// metric, so a test can assert on names, labels and values together.
func gatherMetrics(t *testing.T, snapshot func() Snapshot) string {
	t.Helper()
	registry := prometheus.NewRegistry()
	registry.MustRegister(newCollector(snapshot))
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			labels := make([]string, 0, len(m.GetLabel()))
			for _, l := range m.GetLabel() {
				labels = append(labels, l.GetName()+"="+l.GetValue())
			}
			value := m.GetCounter().GetValue() + m.GetGauge().GetValue()
			body.WriteString(mf.GetName() + "{" + strings.Join(labels, ",") + "} " +
				strconv.FormatFloat(value, 'f', -1, 64) + "\n")
		}
	}
	return body.String()
}

func TestServerMetricsAreLabelledByFamily(t *testing.T) {
	now := time.Date(2026, 8, 24, 23, 37, 21, 0, time.UTC)
	stats := ntpserver.StatsSnapshot{
		Total: ntpserver.CounterSnapshot{Served: 3746, Clients: 412},
		IPv4: ntpserver.CounterSnapshot{
			Served: 3200, Unsynced: 24, KoD: 3, Martian: 3, NonClient: 64,
			Clients: 380, KernelDrops: 7, LastServed: now,
			Modes:    [8]uint64{4: 58, 6: 6},
			Versions: [5]uint64{3: 900, 4: 2300},
		},
		IPv6: ntpserver.CounterSnapshot{Served: 546, Clients: 32},
	}
	body := gatherMetrics(t, func() Snapshot {
		return SnapshotOf(testEngineStatus(now, discipline.StateSynced), stats, true, Metadata{}, now, testPublishedMono, now.Add(-time.Hour))
	})

	for _, want := range []string{
		`carillon_server_requests_total{family=ipv4,result=served} 3200`,
		`carillon_server_requests_total{family=ipv4,result=martian} 3`,
		`carillon_server_requests_total{family=ipv6,result=served} 546`,
		`carillon_server_unsynced_replies_total{family=ipv4} 24`,
		`carillon_server_kod_replies_total{family=ipv4} 3`,
		`carillon_server_refused_mode_total{family=ipv4,mode=control} 6`,
		`carillon_server_refused_mode_total{family=ipv4,mode=server} 58`,
		`carillon_server_client_version_total{family=ipv4,version=4} 2300`,
		`carillon_server_clients{family=ipv4} 380`,
		`carillon_server_clients{family=ipv6} 32`,
		`carillon_server_kernel_drops_total{family=ipv4} 7`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q:\n%s", want, body)
		}
	}
	// Client mode is the request, never a refusal reason, and the decoder
	// rejects version 0 before it can be counted.
	if strings.Contains(body, "mode=client") || strings.Contains(body, "version=0") {
		t.Errorf("impossible label emitted:\n%s", body)
	}
}
