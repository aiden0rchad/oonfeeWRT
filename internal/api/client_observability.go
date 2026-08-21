package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	"github.com/aiden0rchad/oonfeewrt/internal/telemetry"
)

const (
	maxClientObservabilityRange = 31 * 24 * time.Hour
	minObservabilityUnixMillis  = int64(1_000_000_000_000)
	maxClientTimelineEvents     = 2000
)

type observabilityGap struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

type metricAvailability struct {
	State    string             `json:"state"` // available|partial|unavailable
	Source   string             `json:"source"`
	Observed int                `json:"observed_points"`
	Expected int                `json:"expected_points"`
	Gaps     []observabilityGap `json:"gaps"`
	Reason   string             `json:"reason,omitempty"`
}

type clientMetricView struct {
	ID         string             `json:"id"`
	Scope      string             `json:"scope"` // client|ap|site
	Kind       string             `json:"kind"`
	Label      string             `json:"label"`
	Unit       string             `json:"unit"`
	DeviceID   *int64             `json:"device_id,omitempty"`
	DeviceName string             `json:"device_name,omitempty"`
	Key        string             `json:"key,omitempty"`
	Values     []*float64         `json:"values"`
	Mins       []*float64         `json:"mins"`
	Maxs       []*float64         `json:"maxs"`
	Counts     []*int             `json:"counts"`
	Available  metricAvailability `json:"availability"`
}

type metricSeries struct {
	values []*float64
	mins   []*float64
	maxs   []*float64
	counts []*int
}

type clientEventView struct {
	ID         int64           `json:"id"`
	TS         int64           `json:"ts"` // Unix milliseconds, second-resolution source.
	DeviceID   *int64          `json:"device_id,omitempty"`
	Category   string          `json:"category"`
	Severity   string          `json:"severity"`
	Event      string          `json:"event"`
	Detail     json.RawMessage `json:"detail"`
	Source     string          `json:"source"`
	SourceID   string          `json:"source_id,omitempty"`
	SourceBoot string          `json:"source_boot,omitempty"`
	IngestedAt int64           `json:"ingested_at"`
	ClientMAC  string          `json:"client_mac"`
	Action     string          `json:"action,omitempty"`
	Direction  string          `json:"direction,omitempty"`
	InIface    string          `json:"in_iface,omitempty"`
	OutIface   string          `json:"out_iface,omitempty"`
	SrcIP      string          `json:"src_ip,omitempty"`
	DstIP      string          `json:"dst_ip,omitempty"`
	SrcPort    *int            `json:"src_port,omitempty"`
	DstPort    *int            `json:"dst_port,omitempty"`
	ZoneIn     string          `json:"zone_in,omitempty"`
	ZoneOut    string          `json:"zone_out,omitempty"`
	PolicyID   *int64          `json:"policy_id,omitempty"`
}

type observabilityPath struct {
	NodeIDs    []string `json:"node_ids"`
	Labels     []string `json:"labels"`
	Mediums    []string `json:"mediums"`
	Confidence string   `json:"confidence"`
}

type observabilityPathInterval struct {
	From     int64               `json:"from"`
	To       int64               `json:"to"`
	Complete bool                `json:"complete"`
	Paths    []observabilityPath `json:"paths"`
	Gaps     []string            `json:"gaps"`
}

type clientObservabilityResponse struct {
	ClientMAC  string                      `json:"client_mac"`
	From       int64                       `json:"from"`
	To         int64                       `json:"to"`
	Resolution string                      `json:"resolution"`
	BucketMS   int64                       `json:"bucket_ms"`
	Timestamps []int64                     `json:"timestamps"`
	APAt       []*int64                    `json:"ap_device_at"`
	Metrics    []clientMetricView          `json:"metrics"`
	Events     []clientEventView           `json:"events"`
	Paths      []observabilityPathInterval `json:"paths"`
	Gaps       []string                    `json:"gaps"`
	Formula    struct {
		Name          string             `json:"name"`
		Weights       map[string]float64 `json:"weights"`
		MissingPolicy string             `json:"missing_policy"`
	} `json:"experience_formula"`
	DataContract struct {
		MetricSource        string `json:"metric_source"`
		RawSamplesPersisted bool   `json:"raw_samples_persisted"`
		EventTimeResolution int64  `json:"event_time_resolution_ms"`
		EventsTruncated     bool   `json:"events_truncated"`
		TopologySource      string `json:"topology_source"`
	} `json:"data_contract"`
}

type metricDefinition struct {
	kind, label, unit, reason string
}

var clientMetricDefinitions = []metricDefinition{
	{string(telemetry.KindStaRSSI), "Signal", "dBm", "no managed AP reported an RSSI in this rollup bucket"},
	{string(telemetry.KindStaRetryDelta), "TX retry delta", "%", "focused station counters were unavailable, reset, roamed, or had no TX packets"},
	{string(telemetry.KindStaTXFailDelta), "TX failure delta", "%", "focused station counters were unavailable, reset, roamed, or had no TX packets"},
}

var apMetricDefinitions = []metricDefinition{
	{string(telemetry.KindLoad1), "AP load", "load", "no durable AP load rollup exists for this bucket"},
	{string(telemetry.KindMemPct), "AP memory", "%", "no durable AP memory rollup exists for this bucket"},
}

var siteMetricDefinitions = []metricDefinition{
	{string(telemetry.KindSiteWANLatency), "ICMP latency to 1.1.1.1", "ms", "the selected gateway did not complete the fixed ICMP probe for this bucket"},
	{string(telemetry.KindSiteWANLoss), "ICMP loss to 1.1.1.1", "%", "the selected gateway did not complete the fixed ICMP probe for this bucket"},
	{string(telemetry.KindSiteWANUp), "ICMP reachability to 1.1.1.1", "state", "the selected gateway did not complete the fixed ICMP probe for this bucket"},
}

func (s *Server) handleClientObservability(w http.ResponseWriter, r *http.Request) {
	mac, err := canonicalObservabilityMAC(r.PathValue("mac"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "client MAC must be a 48-bit address")
		return
	}
	from, ok := observabilityTimeParam(r, "from")
	if !ok {
		writeErr(w, http.StatusBadRequest, "from must be one Unix-millisecond timestamp")
		return
	}
	to, ok := observabilityTimeParam(r, "to")
	if !ok {
		writeErr(w, http.StatusBadRequest, "to must be one Unix-millisecond timestamp")
		return
	}
	if to <= from {
		writeErr(w, http.StatusBadRequest, "client observability requires from before to")
		return
	}
	if to-from > maxClientObservabilityRange.Milliseconds() {
		writeErr(w, http.StatusBadRequest, "client observability range cannot exceed 31 days")
		return
	}
	exists, err := s.Store.ClientExists(r.Context(), mac)
	if handleStoreErr(w, err, "client") {
		return
	}
	if !exists {
		writeErr(w, http.StatusNotFound, "client not found")
		return
	}

	response, err := s.clientObservability(r, mac, from, to)
	if handleStoreErr(w, err, "client observability") {
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func observabilityTimeParam(r *http.Request, name string) (int64, bool) {
	values, exists := r.URL.Query()[name]
	if !exists || len(values) != 1 || values[0] == "" {
		return 0, false
	}
	value, err := strconv.ParseInt(values[0], 10, 64)
	return value, err == nil && value >= minObservabilityUnixMillis
}

func (s *Server) clientObservability(r *http.Request, mac string, from, to int64) (*clientObservabilityResponse, error) {
	ctx := r.Context()
	kinds := make([]string, 0, len(clientMetricDefinitions))
	for _, definition := range clientMetricDefinitions {
		kinds = append(kinds, definition.kind)
	}
	kinds = append(kinds, string(telemetry.KindStaExperienceWiFiV1))
	clientRows, resolution, bucketMS, err := s.Store.QueryObservabilityRollups(ctx,
		store.ObservabilityRollupQuery{Kinds: kinds, Key: &mac, From: from, To: to})
	if err != nil {
		return nil, err
	}
	timestamps := observabilityTimestamps(from, to, bucketMS)
	indexByTS := make(map[int64]int, len(timestamps))
	for i, ts := range timestamps {
		indexByTS[ts] = i
	}

	clientValues := map[string]map[int64]map[int64]store.ObservabilityRollup{}
	devicesAt := map[int64]map[int64]bool{}
	apSet := map[int64]bool{}
	for _, row := range clientRows {
		if _, exists := indexByTS[row.TS]; !exists {
			continue
		}
		if clientValues[row.Kind] == nil {
			clientValues[row.Kind] = map[int64]map[int64]store.ObservabilityRollup{}
		}
		if clientValues[row.Kind][row.TS] == nil {
			clientValues[row.Kind][row.TS] = map[int64]store.ObservabilityRollup{}
		}
		clientValues[row.Kind][row.TS][row.DeviceID] = row
		if devicesAt[row.TS] == nil {
			devicesAt[row.TS] = map[int64]bool{}
		}
		devicesAt[row.TS][row.DeviceID], apSet[row.DeviceID] = true, true
	}
	apAt := make([]*int64, len(timestamps))
	ambiguousBuckets := 0
	for i, ts := range timestamps {
		if len(devicesAt[ts]) == 1 {
			for deviceID := range devicesAt[ts] {
				apAt[i] = int64Ptr(deviceID)
			}
		} else if len(devicesAt[ts]) > 1 {
			ambiguousBuckets++
		}
	}

	response := &clientObservabilityResponse{
		ClientMAC: mac, From: from, To: to, Resolution: resolution,
		BucketMS: bucketMS, Timestamps: timestamps, APAt: apAt,
		Metrics: []clientMetricView{}, Events: []clientEventView{},
		Paths: []observabilityPathInterval{}, Gaps: []string{},
	}
	response.Formula.Name = telemetry.ExperienceFormula
	response.Formula.Weights = map[string]float64{"rssi": .45, "retry_delta": .35, "tx_fail_delta": .20}
	response.Formula.MissingPolicy = "null when any RSSI, retry-delta, or TX-fail-delta input is missing; weights are never renormalized"
	response.DataContract.MetricSource = "rollup_" + resolution
	response.DataContract.RawSamplesPersisted = false
	response.DataContract.EventTimeResolution = 1000
	response.DataContract.TopologySource = "persisted validity intervals"
	if ambiguousBuckets > 0 {
		response.Gaps = append(response.Gaps, fmt.Sprintf("client AP attribution is ambiguous in %d rollup buckets", ambiguousBuckets))
	}

	for _, definition := range clientMetricDefinitions {
		series := selectedClientSeries(timestamps, apAt, clientValues[definition.kind])
		response.Metrics = append(response.Metrics, makeMetric(
			"client:"+definition.kind, "client", definition, nil, "", "", series,
			resolution, bucketMS, timestamps, to))
	}
	experience := metricDefinition{string(telemetry.KindStaExperienceWiFiV1), "WiFi experience", "score",
		"no persisted wifi-v1 sample existed with RSSI, retry delta, and TX failure delta together"}
	experienceValues := selectedClientSeries(timestamps, apAt, clientValues[experience.kind])
	response.Metrics = append(response.Metrics, makeMetric(
		"client:"+experience.kind, "client", experience, nil, "", "", experienceValues,
		resolution, bucketMS, timestamps, to))

	devices, err := s.Store.Devices(ctx)
	if err != nil {
		return nil, err
	}
	deviceNames := map[int64]string{}
	gatewaySet := map[int64]bool{}
	for _, device := range devices {
		deviceNames[device.ID] = deviceDisplayName(device)
		if device.Adopted() && model.DeviceFunctionsOf(device.Functions, device.Role).Routes() {
			gatewaySet[device.ID] = true
		}
	}

	edges, topologyTruncated, err := s.Store.TopologyEdgesBetween(
		ctx, from, to, maxTopologyHistoryEdges)
	if err != nil {
		return nil, err
	}
	if topologyTruncated || from < s.now().Add(-maxTopologyHistory).UnixMilli() {
		response.Gaps = append(response.Gaps,
			"topology history is truncated by retention or the 10000-interval response limit")
	}
	clientNode := "client:" + mac
	for _, edge := range edges {
		if edge.ChildNode == clientNode && edge.ParentDeviceID != nil {
			apSet[*edge.ParentDeviceID] = true
		}
	}
	response.Paths = buildClientPathIntervals(clientNode, from, to, edges, devices)
	// Source-state rows describe the latest poll only. This joined response is
	// historical, so borrowing them would attach a later failure to an earlier
	// incident window. Historical source-state snapshots are not persisted.
	response.Gaps = append(response.Gaps, "historical topology source coverage is unavailable")

	incidentWindows := clientIncidentWindows(response.Paths, devices)
	if len(incidentWindows) > maxTopologyHistoryEdges {
		incidentWindows = incidentWindows[:maxTopologyHistoryEdges]
		response.Gaps = append(response.Gaps,
			"path-device incident coverage was truncated at 10000 intervals")
	}
	events, truncated, err := s.Store.ClientEventsBetween(ctx, mac, incidentWindows,
		from, to, maxClientTimelineEvents)
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		view, err := makeClientEventView(event)
		if err != nil {
			return nil, err
		}
		response.Events = append(response.Events, view)
		if event.DeviceID != nil &&
			(event.Action == "connect" || event.Action == "disconnect" || event.Action == "roam") {
			apSet[*event.DeviceID] = true
		}
	}
	response.DataContract.EventsTruncated = truncated
	response.Gaps = append(response.Gaps, "historical router-log source coverage is unavailable")
	if truncated {
		response.Gaps = append(response.Gaps, fmt.Sprintf("client events were truncated at %d rows", maxClientTimelineEvents))
	}

	apIDs := sortedIDs(apSet)
	if len(apIDs) == 0 {
		for _, definition := range apMetricDefinitions {
			response.Metrics = append(response.Metrics, makeMetric(
				"ap:unknown:"+definition.kind, "ap", definition, nil, "", "", nilMetricSeries(len(timestamps)),
				resolution, bucketMS, timestamps, to))
		}
		response.Gaps = append(response.Gaps, "no managed AP could be attributed to this client in the requested interval")
	} else {
		apKinds := []string{string(telemetry.KindLoad1), string(telemetry.KindMemPct), string(telemetry.KindRadioUtilization)}
		apRows, _, _, err := s.Store.QueryObservabilityRollups(ctx,
			store.ObservabilityRollupQuery{DeviceIDs: apIDs, Kinds: apKinds, From: from, To: to})
		if err != nil {
			return nil, err
		}
		rowIndex := indexRollups(apRows)
		for _, deviceID := range apIDs {
			for _, definition := range apMetricDefinitions {
				values := indexedSeries(timestamps, rowIndex, deviceID, definition.kind, "")
				response.Metrics = append(response.Metrics, makeMetric(
					fmt.Sprintf("ap:%d:%s", deviceID, definition.kind), "ap", definition,
					int64Ptr(deviceID), deviceNames[deviceID], "", values,
					resolution, bucketMS, timestamps, to))
			}
		}
		radioDefinition := metricDefinition{string(telemetry.KindRadioUtilization), "Channel utilization", "%",
			"the AP radio did not report a portable survey delta for this bucket"}
		radioKeysByDevice := map[int64]map[string]bool{}
		for _, identity := range observedMetricIdentities(apRows, radioDefinition.kind) {
			if radioKeysByDevice[identity.deviceID] == nil {
				radioKeysByDevice[identity.deviceID] = map[string]bool{}
			}
			radioKeysByDevice[identity.deviceID][identity.key] = true
		}
		if provider, ok := s.Fleet.(radioFleet); ok {
			for _, deviceID := range apIDs {
				states, known := provider.Radios(deviceID)
				if !known {
					continue
				}
				if radioKeysByDevice[deviceID] == nil {
					radioKeysByDevice[deviceID] = map[string]bool{}
				}
				for _, state := range states {
					if _, exists := radioKeysByDevice[deviceID][state.Key]; state.Key != "" && !exists {
						radioKeysByDevice[deviceID][state.Key] = false
					}
				}
			}
		}
		for _, deviceID := range apIDs {
			radioKeys := radioKeysByDevice[deviceID]
			if len(radioKeys) == 0 {
				unavailable := radioDefinition
				unavailable.reason = "no stored stable-radio channel-utilization rollup exists for this attributed AP in the requested interval"
				response.Metrics = append(response.Metrics, makeMetric(
					fmt.Sprintf("ap:%d:%s", deviceID, unavailable.kind), "ap", unavailable,
					int64Ptr(deviceID), deviceNames[deviceID], "", nilMetricSeries(len(timestamps)),
					resolution, bucketMS, timestamps, to))
				continue
			}
			keys := make([]string, 0, len(radioKeys))
			for key := range radioKeys {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				definition := radioDefinition
				if !radioKeys[key] {
					definition.reason = "no stored stable-radio channel-utilization rollup exists for this known radio in the requested interval"
				}
				values := indexedSeries(timestamps, rowIndex, deviceID, radioDefinition.kind, key)
				response.Metrics = append(response.Metrics, makeMetric(
					fmt.Sprintf("ap:%d:%s:%s", deviceID, radioDefinition.kind, key), "ap", definition,
					int64Ptr(deviceID), deviceNames[deviceID], key, values,
					resolution, bucketMS, timestamps, to))
			}
		}
	}

	gatewayIDs := sortedIDs(gatewaySet)
	if len(gatewayIDs) == 0 {
		for _, definition := range siteMetricDefinitions {
			response.Metrics = append(response.Metrics, makeMetric(
				"site:unknown:"+definition.kind, "site", definition, nil, "", "", nilMetricSeries(len(timestamps)),
				resolution, bucketMS, timestamps, to))
		}
		response.Gaps = append(response.Gaps, "no adopted gateway is available as site-health provenance")
	} else {
		siteKinds := make([]string, 0, len(siteMetricDefinitions))
		for _, definition := range siteMetricDefinitions {
			siteKinds = append(siteKinds, definition.kind)
		}
		emptyKey := ""
		siteRows, _, _, err := s.Store.QueryObservabilityRollups(ctx,
			store.ObservabilityRollupQuery{DeviceIDs: gatewayIDs, Kinds: siteKinds, Key: &emptyKey, From: from, To: to})
		if err != nil {
			return nil, err
		}
		rowIndex := indexRollups(siteRows)
		for _, deviceID := range gatewayIDs {
			for _, definition := range siteMetricDefinitions {
				values := indexedSeries(timestamps, rowIndex, deviceID, definition.kind, "")
				response.Metrics = append(response.Metrics, makeMetric(
					fmt.Sprintf("site:%d:%s", deviceID, definition.kind), "site", definition,
					int64Ptr(deviceID), deviceNames[deviceID], "", values,
					resolution, bucketMS, timestamps, to))
			}
		}
	}

	response.Gaps = uniqueTopologyStrings(response.Gaps)
	return response, nil
}

func selectedClientSeries(timestamps []int64, apAt []*int64,
	values map[int64]map[int64]store.ObservabilityRollup) metricSeries {
	out := nilMetricSeries(len(timestamps))
	for i, ts := range timestamps {
		if apAt[i] == nil {
			continue
		}
		if row, ok := values[ts][*apAt[i]]; ok {
			setMetricBucket(&out, i, row)
		}
	}
	return out
}

func makeMetric(id, scope string, definition metricDefinition, deviceID *int64,
	deviceName, key string, series metricSeries, source string, bucketMS int64,
	timestamps []int64, to int64) clientMetricView {
	if series.values == nil {
		series = nilMetricSeries(len(timestamps))
	}
	return clientMetricView{
		ID: id, Scope: scope, Kind: definition.kind, Label: definition.label,
		Unit: definition.unit, DeviceID: deviceID, DeviceName: deviceName, Key: key,
		Values: series.values, Mins: series.mins, Maxs: series.maxs, Counts: series.counts,
		Available: metricAvailabilityFor(series.values, source, definition.reason, timestamps, bucketMS, to),
	}
}

func metricAvailabilityFor(values []*float64, source, reason string, timestamps []int64,
	bucketMS, to int64) metricAvailability {
	availability := metricAvailability{
		State: "unavailable", Source: source, Expected: len(values),
		Gaps: []observabilityGap{}, Reason: reason,
	}
	for _, value := range values {
		if value != nil {
			availability.Observed++
		}
	}
	switch {
	case availability.Observed == len(values) && len(values) > 0:
		availability.State, availability.Reason = "available", ""
	case availability.Observed > 0:
		availability.State = "partial"
	}
	for start := 0; start < len(values); {
		if values[start] != nil {
			start++
			continue
		}
		end := start + 1
		for end < len(values) && values[end] == nil {
			end++
		}
		gapTo := timestamps[end-1] + bucketMS
		if gapTo > to {
			gapTo = to
		}
		availability.Gaps = append(availability.Gaps, observabilityGap{From: timestamps[start], To: gapTo})
		start = end
	}
	return availability
}

type rollupIdentity struct {
	deviceID int64
	kind     string
	key      string
}

func indexRollups(rows []store.ObservabilityRollup) map[rollupIdentity]map[int64]store.ObservabilityRollup {
	index := map[rollupIdentity]map[int64]store.ObservabilityRollup{}
	for _, row := range rows {
		identity := rollupIdentity{row.DeviceID, row.Kind, row.Key}
		if index[identity] == nil {
			index[identity] = map[int64]store.ObservabilityRollup{}
		}
		index[identity][row.TS] = row
	}
	return index
}

func indexedSeries(timestamps []int64, index map[rollupIdentity]map[int64]store.ObservabilityRollup,
	deviceID int64, kind, key string) metricSeries {
	out := nilMetricSeries(len(timestamps))
	values := index[rollupIdentity{deviceID, kind, key}]
	for i, ts := range timestamps {
		if row, ok := values[ts]; ok {
			setMetricBucket(&out, i, row)
		}
	}
	return out
}

func observedMetricIdentities(rows []store.ObservabilityRollup, kind string) []rollupIdentity {
	seen := map[rollupIdentity]bool{}
	for _, row := range rows {
		if row.Kind == kind {
			seen[rollupIdentity{row.DeviceID, row.Kind, row.Key}] = true
		}
	}
	out := make([]rollupIdentity, 0, len(seen))
	for identity := range seen {
		out = append(out, identity)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].deviceID != out[j].deviceID {
			return out[i].deviceID < out[j].deviceID
		}
		return out[i].key < out[j].key
	})
	return out
}

func makeClientEventView(event store.Event) (clientEventView, error) {
	detail, ok := event.Detail.(json.RawMessage)
	if !ok {
		var err error
		detail, err = json.Marshal(event.Detail)
		if err != nil {
			return clientEventView{}, err
		}
	}
	return clientEventView{
		ID: event.ID, TS: event.TS * 1000, DeviceID: event.DeviceID,
		Category: event.Category, Severity: event.Severity, Event: event.Event,
		Detail: detail, Source: event.Source, SourceID: event.SourceID,
		SourceBoot: event.SourceBoot, IngestedAt: event.IngestedAt,
		ClientMAC: event.ClientMAC, Action: event.Action, Direction: event.Direction,
		InIface: event.InIface, OutIface: event.OutIface, SrcIP: event.SrcIP,
		DstIP: event.DstIP, SrcPort: event.SrcPort, DstPort: event.DstPort,
		ZoneIn: event.ZoneIn, ZoneOut: event.ZoneOut, PolicyID: event.PolicyID,
	}, nil
}

func clientIncidentWindows(intervals []observabilityPathInterval,
	devices []*store.Device) []store.ClientIncidentWindow {
	nodeDevice := map[string]int64{}
	for _, device := range devices {
		mac, err := canonicalObservabilityMAC(device.MAC)
		if err == nil {
			nodeDevice["device:"+mac] = device.ID
		}
	}
	byDevice := map[int64][]store.ClientIncidentWindow{}
	for _, interval := range intervals {
		present := map[int64]bool{}
		for _, path := range interval.Paths {
			for _, node := range path.NodeIDs {
				if id := nodeDevice[node]; id > 0 {
					present[id] = true
				}
			}
		}
		for _, id := range sortedIDs(present) {
			windows := byDevice[id]
			if len(windows) > 0 && windows[len(windows)-1].To == interval.From {
				windows[len(windows)-1].To = interval.To
				byDevice[id] = windows
				continue
			}
			byDevice[id] = append(windows, store.ClientIncidentWindow{
				DeviceID: id, From: interval.From, To: interval.To,
			})
		}
	}
	out := []store.ClientIncidentWindow{}
	deviceIDs := make(map[int64]bool, len(byDevice))
	for id := range byDevice {
		deviceIDs[id] = true
	}
	for _, id := range sortedIDs(deviceIDs) {
		out = append(out, byDevice[id]...)
	}
	return out
}

func observabilityTimestamps(from, to, bucketMS int64) []int64 {
	start := from
	if remainder := from % bucketMS; remainder != 0 {
		delta := bucketMS - remainder
		if from > math.MaxInt64-delta {
			return []int64{}
		}
		start += delta
	}
	end := to / bucketMS * bucketMS
	out := []int64{}
	for ts := start; ts < end; {
		out = append(out, ts)
		// start and end are aligned, so this cannot overflow while ts < end.
		ts += bucketMS
	}
	return out
}

func sortedIDs(set map[int64]bool) []int64 {
	out := make([]int64, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func nilMetricSeries(n int) metricSeries {
	return metricSeries{
		values: make([]*float64, n), mins: make([]*float64, n),
		maxs: make([]*float64, n), counts: make([]*int, n),
	}
}

func setMetricBucket(series *metricSeries, i int, row store.ObservabilityRollup) {
	series.values[i], series.mins[i], series.maxs[i], series.counts[i] =
		float64Ptr(row.Avg), float64Ptr(row.Min), float64Ptr(row.Max), intPtr(row.Cnt)
}

func int64Ptr(value int64) *int64       { return &value }
func intPtr(value int) *int             { return &value }
func float64Ptr(value float64) *float64 { return &value }

func canonicalObservabilityMAC(raw string) (string, error) {
	mac, err := net.ParseMAC(strings.TrimSpace(raw))
	if err != nil || len(mac) != 6 {
		return "", fmt.Errorf("invalid 48-bit MAC address")
	}
	return strings.ToLower(mac.String()), nil
}
