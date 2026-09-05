package monitor

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"carillon/internal/control"
)

type collector struct {
	snapshot func() Snapshot

	state          *prometheus.Desc
	offset         *prometheus.Desc
	frequency      *prometheus.Desc
	jitter         *prometheus.Desc
	rootDispersion *prometheus.Desc
	stratum        *prometheus.Desc
	steps          *prometheus.Desc
	updates        *prometheus.Desc
	leapPending    *prometheus.Desc
	leapfileExpiry *prometheus.Desc
	leapfileValid  *prometheus.Desc
	buildInfo      *prometheus.Desc

	sourceOffset     *prometheus.Desc
	sourceDelay      *prometheus.Desc
	sourceJitter     *prometheus.Desc
	sourceDistance   *prometheus.Desc
	sourceReach      *prometheus.Desc
	sourceSelected   *prometheus.Desc
	sourceLastRx     *prometheus.Desc
	sourceNoKernelTS *prometheus.Desc
	sourceEvents     *prometheus.Desc

	ppsSamples *prometheus.Desc
	ppsJitter  *prometheus.Desc
	ppsLocked  *prometheus.Desc
	gpsFix     *prometheus.Desc
	gpsSats    *prometheus.Desc
	gpsLag     *prometheus.Desc

	serverEnabled     *prometheus.Desc
	serverRequests    *prometheus.Desc
	serverUnsynced    *prometheus.Desc
	serverKoD         *prometheus.Desc
	serverModes       *prometheus.Desc
	serverVersions    *prometheus.Desc
	serverClients     *prometheus.Desc
	serverKernelDrops *prometheus.Desc
	serverLastRequest *prometheus.Desc
	serverLastServed  *prometheus.Desc
	serverNoKernelTS  *prometheus.Desc
}

func newMetricsHandler(snapshot func() Snapshot) http.Handler {
	registry := prometheus.NewRegistry()
	registry.MustRegister(newCollector(snapshot))
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{EnableOpenMetrics: true})
}

func newCollector(snapshot func() Snapshot) *collector {
	desc := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(name, help, labels, nil)
	}
	return &collector{
		snapshot: snapshot,

		state:          desc("carillon_state", "Current discipline state as a one-hot labeled gauge.", "state"),
		offset:         desc("carillon_offset_seconds", "Current system offset estimate in seconds."),
		frequency:      desc("carillon_frequency_ppm", "Current kernel frequency correction in parts per million."),
		jitter:         desc("carillon_jitter_seconds", "Current system jitter estimate in seconds."),
		rootDispersion: desc("carillon_root_dispersion_seconds", "Current NTP root dispersion in seconds."),
		stratum:        desc("carillon_stratum", "Current NTP stratum."),
		steps:          desc("carillon_steps_total", "Clock steps since this daemon process started."),
		updates:        desc("carillon_updates_total", "Discipline updates since this daemon process started."),
		leapPending:    desc("carillon_leap_pending", "Whether an insertion or deletion leap second is pending."),
		leapfileExpiry: desc("carillon_leapfile_expiry_timestamp_seconds", "Unix timestamp at which the configured leap file expires; zero when absent."),
		leapfileValid:  desc("carillon_leapfile_valid", "Whether the configured leap file is unexpired; zero when absent or expired."),
		buildInfo:      desc("carillon_build_info", "Build information for this carillon process.", "version"),

		sourceOffset:     desc("carillon_source_offset_seconds", "Filtered source offset estimate in seconds.", "source"),
		sourceDelay:      desc("carillon_source_delay_seconds", "Filtered source round-trip delay in seconds.", "source"),
		sourceJitter:     desc("carillon_source_jitter_seconds", "Filtered source jitter in seconds.", "source"),
		sourceDistance:   desc("carillon_source_root_distance_seconds", "Source correctness interval radius in seconds.", "source"),
		sourceReach:      desc("carillon_source_reach", "Eight-bit source reach register as an integer.", "source"),
		sourceSelected:   desc("carillon_source_selected", "Whether this source currently drives the system clock.", "source"),
		sourceLastRx:     desc("carillon_source_receive_timestamp_seconds", "Unix timestamp of the last accepted source reply or pulse.", "source"),
		sourceNoKernelTS: desc("carillon_source_kernel_timestamp_missing_total", "Source replies received without a kernel timestamp.", "source"),
		sourceEvents:     desc("carillon_source_events_total", "Source protocol event counters.", "source", "result"),

		ppsSamples: desc("carillon_pps_samples_total", "PPS sample and rejection counters.", "source", "result"),
		ppsJitter:  desc("carillon_pps_jitter_seconds", "Robust jitter of the current PPS sample window.", "source"),
		ppsLocked:  desc("carillon_pps_locked", "Whether PPS is stable and qualified to discipline the clock.", "source"),
		gpsFix:     desc("carillon_gps_fix_valid", "Whether the GPS receiver reports a valid fix.", "source"),
		gpsSats:    desc("carillon_gps_satellites", "Satellites used by the GPS receiver.", "source"),
		gpsLag:     desc("carillon_gps_nmea_lag_seconds", "Measured lag from the latest PPS edge to NMEA sentence arrival.", "source"),

		serverEnabled:     desc("carillon_server_enabled", "Whether the NTP listener is configured."),
		serverRequests:    desc("carillon_server_requests_total", "Datagrams the NTP listener received, partitioned by outcome; summing over result gives all received traffic.", "family", "result"),
		serverUnsynced:    desc("carillon_server_unsynced_replies_total", "Replies served while the clock was not synchronized; a subset of result=served.", "family"),
		serverKoD:         desc("carillon_server_kod_replies_total", "Rate kiss-o'-death replies sent; a subset of result=rate_limited.", "family"),
		serverModes:       desc("carillon_server_refused_mode_total", "Refused datagrams by NTP association mode; modes 6 and 7 are amplification probes.", "family", "mode"),
		serverVersions:    desc("carillon_server_client_version_total", "Client requests by NTP protocol version, counted after the ACL, the rate limiter and authentication: what was actually answered.", "family", "version"),
		serverClients:     desc("carillon_server_clients", "Rate-limit buckets in use as of the last request served: one per IPv4 address, one per rate_limit_v6_prefix (default /64) for IPv6, and a separate one per authenticated key id. Entries expire after one minute, so this is roughly the clients seen in the last minute and goes stale on an idle server.", "family"),
		serverKernelDrops: desc("carillon_server_kernel_drops_total", "Datagrams the kernel dropped because the socket receive queue was full; Linux only, FreeBSD reports no per-socket count.", "family"),
		serverLastRequest: desc("carillon_server_last_request_timestamp_seconds", "Unix timestamp of the last valid NTP client request.", "family"),
		serverLastServed:  desc("carillon_server_last_served_timestamp_seconds", "Unix timestamp of the last NTP response served.", "family"),
		serverNoKernelTS:  desc("carillon_server_kernel_timestamp_missing_total", "NTP requests received without a kernel timestamp.", "family"),
	}
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		c.state, c.offset, c.frequency, c.jitter, c.rootDispersion, c.stratum,
		c.steps, c.updates, c.leapPending, c.leapfileExpiry, c.leapfileValid, c.buildInfo,
		c.sourceOffset, c.sourceDelay, c.sourceJitter, c.sourceDistance,
		c.sourceReach, c.sourceSelected, c.sourceLastRx, c.sourceNoKernelTS, c.sourceEvents,
		c.ppsSamples, c.ppsJitter, c.ppsLocked, c.gpsFix, c.gpsSats, c.gpsLag,
		c.serverEnabled, c.serverRequests, c.serverUnsynced, c.serverKoD,
		c.serverModes, c.serverVersions, c.serverClients, c.serverKernelDrops,
		c.serverLastRequest, c.serverLastServed, c.serverNoKernelTS,
	} {
		ch <- d
	}
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	s := c.snapshot()
	t := s.Tracking
	gauge := func(d *prometheus.Desc, value float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, value, labels...)
	}
	counter := func(d *prometheus.Desc, value uint64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, float64(value), labels...)
	}
	boolValue := func(v bool) float64 {
		if v {
			return 1
		}
		return 0
	}
	timestamp := timestampSeconds

	gauge(c.state, 1, t.State)
	gauge(c.offset, t.Offset)
	gauge(c.frequency, t.Frequency)
	gauge(c.jitter, t.Jitter)
	gauge(c.rootDispersion, t.RootDisp)
	gauge(c.stratum, float64(t.Stratum))
	counter(c.steps, uint64(t.Steps))
	counter(c.updates, uint64(t.Updates))
	gauge(c.leapPending, boolValue(t.Leap == "insert" || t.Leap == "delete"))
	gauge(c.leapfileExpiry, timestamp(t.LeapExpiry))
	gauge(c.leapfileValid, boolValue(!t.LeapExpiry.IsZero() && t.LeapExpiry.After(t.Now)))
	gauge(c.buildInfo, 1, t.Version)

	for _, src := range s.Sources {
		gauge(c.sourceOffset, src.Offset, src.Name)
		gauge(c.sourceDelay, src.Delay, src.Name)
		gauge(c.sourceJitter, src.Jitter, src.Name)
		gauge(c.sourceDistance, src.Distance, src.Name)
		gauge(c.sourceReach, float64(src.Reach), src.Name)
		gauge(c.sourceSelected, boolValue(src.Status == "system"), src.Name)
		gauge(c.sourceLastRx, timestamp(src.LastRx), src.Name)
		counter(c.sourceNoKernelTS, src.NoKernelTS, src.Name)
		for result, value := range map[string]uint64{
			"sent": src.Sent, "received": src.Received, "timeout": src.Timeouts,
			"bogus": src.Bogus, "bad_auth": src.BadAuth, "kiss": src.Kiss,
			"stale": src.Stale,
		} {
			counter(c.sourceEvents, value, src.Name, result)
		}
	}

	for _, ref := range s.Refclocks {
		if ref.Type == "pps" || ref.Type == "gps-pps" {
			for result, value := range map[string]uint64{
				"ok": ref.Samples, "timeout": ref.Timeouts, "gap": ref.Gaps,
				"glitch": ref.Glitches, "spike": ref.Spikes,
			} {
				counter(c.ppsSamples, value, ref.Name, result)
			}
			gauge(c.ppsJitter, ref.WindowJitter, ref.Name)
			gauge(c.ppsLocked, boolValue(ref.Locked), ref.Name)
		}
		if ref.Type == "gps-nmea" {
			gauge(c.gpsFix, boolValue(ref.FixValid), ref.Name)
			gauge(c.gpsSats, float64(ref.Satellites), ref.Name)
			gauge(c.gpsLag, ref.MeasuredLag, ref.Name)
		}
	}

	gauge(c.serverEnabled, boolValue(s.Server.Enabled))
	// Families are reported separately rather than as a total: the NTP pool
	// scores IPv4 and IPv6 as two monitors, and Prometheus sums the label
	// away whenever the total is what is wanted.
	c.collectFamily(ch, "ipv4", &s.Server.IPv4)
	c.collectFamily(ch, "ipv6", &s.Server.IPv6)
}

// serverResults partitions received datagrams by outcome. Every datagram the
// listener read increments exactly one of these, so summing over the result
// label gives total received traffic without double counting.
func serverResults(v *control.CounterStats) map[string]uint64 {
	return map[string]uint64{
		"served":       v.Served,
		"denied":       v.Denied,
		"martian":      v.Martian,
		"rate_limited": v.RateLimited,
		"bad_auth":     v.BadAuth,
		"bad_version":  v.BadVersion,
		"non_client":   v.NonClient,
		"malformed":    v.Malformed,
		"oversize":     v.Oversize,
	}
}

func (c *collector) collectFamily(ch chan<- prometheus.Metric, family string, v *control.CounterStats) {
	counter := func(d *prometheus.Desc, value uint64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, float64(value), labels...)
	}
	gauge := func(d *prometheus.Desc, value float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, value, labels...)
	}
	for result, value := range serverResults(v) {
		counter(c.serverRequests, value, family, result)
	}
	counter(c.serverUnsynced, v.Unsynced, family)
	counter(c.serverKoD, v.KoD, family)
	// Both histograms carry only their non-zero buckets, so a name absent
	// from the map has never been seen and is exported as zero. Exporting
	// the whole enum keeps a series from appearing mid-scrape.
	for _, mode := range control.ModeOrder {
		counter(c.serverModes, v.Modes[mode], family, mode)
	}
	for _, version := range control.VersionOrder {
		counter(c.serverVersions, v.Versions[version], family, version)
	}
	gauge(c.serverClients, float64(v.Clients), family)
	counter(c.serverKernelDrops, v.KernelDrops, family)
	counter(c.serverNoKernelTS, v.NoKernelTS, family)
	gauge(c.serverLastRequest, optionalTimestampSeconds(v.LastRequest), family)
	gauge(c.serverLastServed, optionalTimestampSeconds(v.LastServed), family)
}

func timestampSeconds(v time.Time) float64 {
	if v.IsZero() {
		return 0
	}
	return float64(v.UnixNano()) / 1e9
}

// optionalTimestampSeconds exports an absent time as zero, which is how every
// other "never happened" timestamp in this exposition reads.
func optionalTimestampSeconds(v *time.Time) float64 {
	if v == nil {
		return 0
	}
	return timestampSeconds(*v)
}
