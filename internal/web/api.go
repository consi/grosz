package web

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"

	"github.com/consi/grosz/internal/scheduler"
	"github.com/consi/grosz/internal/store"
	"github.com/consi/grosz/internal/tariff"
)

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.buildStatus())
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	n := queryInt(r, "n", 100)
	offset := queryInt(r, "offset", 0)
	action := r.URL.Query().Get("action")

	var events any
	var err error
	if action != "" {
		events, err = s.store.EventsByAction(action, n, offset)
	} else {
		events, err = s.store.RecentEvents(n, offset)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	total, _ := s.store.EventCount(action)

	writeJSON(w, map[string]any{"events": events, "total": total})
}

func (s *Server) handleSystemEvents(w http.ResponseWriter, r *http.Request) {
	n := queryInt(r, "n", 100)
	offset := queryInt(r, "offset", 0)
	source := r.URL.Query().Get("source")

	var events any
	var err error
	if source != "" {
		events, err = s.store.SystemEventsBySource(source, n, offset)
	} else {
		events, err = s.store.RecentSystemEvents(n, offset)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	total, _ := s.store.SystemEventCount(source)

	writeJSON(w, map[string]any{"events": events, "total": total})
}

func (s *Server) handleRates(w http.ResponseWriter, r *http.Request) {
	rates, err := s.tariff.Rates()
	if err != nil {
		writeJSON(w, map[string]any{"rates": []any{}, "error": err.Error()})
		return
	}
	// Filter out rates with zero price (data not yet available)
	filtered := make([]tariff.Rate, 0, len(rates))
	for _, r := range rates {
		if r.Price != 0 {
			filtered = append(filtered, r)
		}
	}

	// Load yesterday's rates from DB to show past data
	now := time.Now()
	yesterdayMidnight := time.Date(now.Year(), now.Month(), now.Day()-1, 0, 0, 0, 0, now.Location())
	var earliest time.Time
	if len(filtered) > 0 {
		earliest = filtered[0].Start
	} else {
		earliest = now
	}
	if yesterdayMidnight.Before(earliest) {
		historical, err := s.store.RatesForPeriod(yesterdayMidnight, earliest)
		if err == nil && len(historical) > 0 {
			// Convert store.Rate to tariff.Rate and prepend
			past := make([]tariff.Rate, 0, len(historical))
			for _, hr := range historical {
				if hr.Price != 0 {
					past = append(past, tariff.Rate{Start: hr.Start, End: hr.End, Price: hr.Price})
				}
			}
			filtered = append(past, filtered...)
		}
	}

	writeJSON(w, map[string]any{"rates": filtered, "provider": s.tariff.Name()})
}

func (s *Server) handleGetSchedule(w http.ResponseWriter, r *http.Request) {
	if s.scheduler == nil {
		writeJSON(w, map[string]any{"schedule": nil})
		return
	}
	sched := s.scheduler.Schedule()
	writeJSON(w, map[string]any{"schedule": sched})
}

func (s *Server) handleSetSchedule(w http.ResponseWriter, r *http.Request) {
	if s.scheduler == nil {
		http.Error(w, "scheduler not enabled", http.StatusServiceUnavailable)
		return
	}

	deadlineStr := s.store.GetDefault("scheduler.deadline_time", "07:00")
	now := time.Now()
	var h, m int
	if _, err := time.Parse("15:04", deadlineStr); err == nil {
		h, _ = strconv.Atoi(deadlineStr[:2])
		m, _ = strconv.Atoi(deadlineStr[3:5])
	}
	deadline := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if deadline.Before(now) {
		deadline = deadline.Add(24 * time.Hour)
	}

	cfg := scheduler.Config{
		TargetEnergy:    s.store.GetFloat("scheduler.target_energy", 30),
		Deadline:        deadline,
		MaxPower:        s.store.GetFloat("charger.max_power", 11000),
		MinPower:        s.store.GetFloat("charger.min_power", 1380),
		BatteryCapacity: s.store.GetFloat("scheduler.battery_capacity", 0),
		TargetSoC:       s.store.GetFloat("scheduler.target_soc", 0),
		SkipAboveSoC:    s.store.GetFloat("scheduler.skip_above_soc", 0),
		CurrentSoC:      s.store.GetFloat("scheduler.current_soc", 0),
		MaxPrice:        s.store.GetFloat("scheduler.max_price", 0),
		ChargeHeadroom:  s.store.GetFloat("scheduler.charge_headroom", 3),
	}
	s.scheduler.SetConfig(cfg)

	writeJSON(w, map[string]any{"ok": true, "schedule": s.scheduler.Schedule()})
}

func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	if s.scheduler != nil {
		s.scheduler.ClearSchedule()
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleCancelSlot(w http.ResponseWriter, r *http.Request) {
	date := r.PathValue("date")
	if date == "" {
		http.Error(w, "date required", http.StatusBadRequest)
		return
	}
	if s.scheduler == nil {
		http.Error(w, "scheduler not enabled", http.StatusServiceUnavailable)
		return
	}
	if !s.scheduler.CancelSlot(date) {
		http.Error(w, "slot not found", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "schedule": s.scheduler.Schedule()})
}

func (s *Server) handleRestoreSlot(w http.ResponseWriter, r *http.Request) {
	date := r.PathValue("date")
	if date == "" {
		http.Error(w, "date required", http.StatusBadRequest)
		return
	}
	if s.scheduler == nil {
		http.Error(w, "scheduler not enabled", http.StatusServiceUnavailable)
		return
	}
	if !s.scheduler.RestoreSlot(date) {
		http.Error(w, "slot not found", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "schedule": s.scheduler.Schedule()})
}

func (s *Server) handleListOverrides(w http.ResponseWriter, r *http.Request) {
	overrides, err := s.store.LoadOverrides(time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if overrides == nil {
		overrides = []store.ScheduleOverride{}
	}
	writeJSON(w, map[string]any{"overrides": overrides})
}

type createOverrideRequest struct {
	Kind   string  `json:"kind"`
	Start  string  `json:"start"`
	End    string  `json:"end"`
	PowerW float64 `json:"powerW"`
}

func (s *Server) handleCreateOverride(w http.ResponseWriter, r *http.Request) {
	var req createOverrideRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	start, err1 := time.Parse(time.RFC3339, req.Start)
	end, err2 := time.Parse(time.RFC3339, req.End)
	if err1 != nil || err2 != nil {
		writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]any{"error": "invalid start or end (RFC3339 required)"})
		return
	}
	if !end.After(start) {
		writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]any{"error": "end must be after start"})
		return
	}
	if !end.After(time.Now()) {
		writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]any{"error": "end must be in the future"})
		return
	}
	if req.Kind != store.OverrideKindForce && req.Kind != store.OverrideKindBlock {
		writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]any{"error": "kind must be 'force' or 'block'"})
		return
	}
	if req.Kind == store.OverrideKindForce {
		if req.PowerW <= 0 {
			writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]any{"error": "force override requires powerW > 0"})
			return
		}
		maxPower := s.store.GetFloat("charger.max_power", 11000)
		if req.PowerW > maxPower {
			writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]any{"error": fmt.Sprintf("powerW %.0f exceeds charger.max_power %.0f", req.PowerW, maxPower)})
			return
		}
	}

	o := store.ScheduleOverride{
		Kind:   req.Kind,
		Start:  start,
		End:    end,
		PowerW: req.PowerW,
	}
	id, err := s.store.InsertOverride(o)
	input := map[string]any{
		"kind":   req.Kind,
		"start":  start.Format(time.RFC3339),
		"end":    end.Format(time.RFC3339),
		"powerW": req.PowerW,
	}
	if err != nil {
		s.store.RecordSystemEvent(store.SystemEvent{
			Timestamp: time.Now(), Source: "scheduler", Action: "createOverride", Level: "warn",
			Input: input, Result: map[string]any{"error": err.Error()},
		})
		if errors.Is(err, store.ErrOverlap) {
			writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "overlaps existing override"})
			return
		}
		writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	o.ID = id

	s.store.RecordSystemEvent(store.SystemEvent{
		Timestamp: time.Now(), Source: "scheduler", Action: "createOverride",
		Input: input, Result: map[string]any{"id": id},
	})

	if s.scheduler != nil {
		s.scheduler.NotifyImmediate()
	}
	writeJSON(w, map[string]any{"ok": true, "override": o, "schedule": s.schedulerSchedule()})
}

func (s *Server) handleDeleteOverride(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := s.store.DeleteOverride(id); err != nil {
		s.store.RecordSystemEvent(store.SystemEvent{
			Timestamp: time.Now(), Source: "scheduler", Action: "deleteOverride", Level: "warn",
			Input: map[string]any{"id": id}, Result: map[string]any{"error": err.Error()},
		})
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.store.RecordSystemEvent(store.SystemEvent{
		Timestamp: time.Now(), Source: "scheduler", Action: "deleteOverride",
		Input: map[string]any{"id": id}, Result: map[string]any{"ok": true},
	})

	if s.scheduler != nil {
		s.scheduler.NotifyImmediate()
	}
	writeJSON(w, map[string]any{"ok": true, "schedule": s.schedulerSchedule()})
}

// schedulerSchedule returns the current schedule, nil-safe.
func (s *Server) schedulerSchedule() *scheduler.Schedule {
	if s.scheduler == nil {
		return nil
	}
	return s.scheduler.Schedule()
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) handleChargerStart(w http.ResponseWriter, r *http.Request) {
	cpID := r.PathValue("id")
	idTag := s.store.GetDefault("zappi.id_tag", "grosz")
	if err := s.ocpp.RemoteStartTransaction(cpID, idTag, 1); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleChargerStop(w http.ResponseWriter, r *http.Request) {
	cpID := r.PathValue("id")

	cp := s.ocpp.ChargePoint(cpID)
	if cp == nil {
		http.Error(w, "charge point not found", http.StatusNotFound)
		return
	}

	snap := cp.Snapshot()
	for _, c := range snap.Connectors {
		if c.TransactionID > 0 {
			if err := s.ocpp.RemoteStopTransaction(cpID, c.TransactionID); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]any{"ok": true})
			return
		}
	}
	http.Error(w, "no active transaction", http.StatusBadRequest)
}

// firmwarePlaceholderURL is the URL we send in UpdateFirmware. Zappi ignores
// the location and pulls firmware from myenergi servers, but the OCPP 1.6
// schema requires a syntactically-valid URI on the request.
const firmwarePlaceholderURL = "https://myenergi.invalid/firmware"

func (s *Server) handleChargerReset(w http.ResponseWriter, r *http.Request) {
	cpID := r.PathValue("id")

	var req struct {
		Type string `json:"type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var resetType core.ResetType
	switch req.Type {
	case string(core.ResetTypeHard):
		resetType = core.ResetTypeHard
	case string(core.ResetTypeSoft):
		resetType = core.ResetTypeSoft
	default:
		http.Error(w, "type must be Hard or Soft", http.StatusBadRequest)
		return
	}

	if err := s.ocpp.Reset(cpID, resetType); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleChargerClearCache(w http.ResponseWriter, r *http.Request) {
	cpID := r.PathValue("id")
	if err := s.ocpp.ClearCache(cpID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleChargerUpdateFirmware(w http.ResponseWriter, r *http.Request) {
	cpID := r.PathValue("id")
	// Zappi ignores the URL and uses myenergi's own firmware servers; we
	// still must send a syntactically-valid URI to satisfy OCPP 1.6 schema
	// validation. retrieveDate must be UTC per Zappi.
	if err := s.ocpp.UpdateFirmware(cpID, firmwarePlaceholderURL, time.Now().UTC()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleMeterHourly(w http.ResponseWriter, r *http.Request) {
	hours := queryInt(r, "hours", 48)
	data, err := s.store.HourlyConsumption(hours)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, data)
}

func (s *Server) handleGetChargerMode(w http.ResponseWriter, r *http.Request) {
	mode := s.store.GetDefault("charger.mode", "schedule")
	writeJSON(w, map[string]any{"mode": mode})
}

func (s *Server) handleSetChargerMode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	switch req.Mode {
	case "off", "schedule", "force":
		// valid
	default:
		http.Error(w, "invalid mode: must be off, schedule, or force", http.StatusBadRequest)
		return
	}

	if err := s.store.Set("charger.mode", req.Mode); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	switch req.Mode {
	case "off", "force":
		if s.scheduler != nil {
			s.scheduler.ClearSchedule()
		}
	}

	// Notify scheduler to enforce the new mode immediately
	if s.scheduler != nil {
		s.scheduler.Notify()
	}

	writeJSON(w, map[string]any{"ok": true, "mode": req.Mode})
}

func (s *Server) handleChartMarkers(w http.ResponseWriter, r *http.Request) {
	hours := queryInt(r, "hours", 72)
	markers, err := s.store.RecentChartMarkers(hours)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if markers == nil {
		markers = []store.ChartMarker{}
	}
	writeJSON(w, markers)
}

func (s *Server) handleMeterPhases(w http.ResponseWriter, r *http.Request) {
	minutes := queryInt(r, "minutes", 60)
	data, err := s.store.RecentPhaseReadings(minutes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, data)
}

func (s *Server) handleMeterLive(w http.ResponseWriter, r *http.Request) {
	if s.meter == nil {
		writeJSON(w, map[string]any{"error": "meter not configured"})
		return
	}
	writeJSON(w, s.meter.Live())
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 50)
	offset := queryInt(r, "offset", 0)
	fromStr := r.URL.Query().Get("from")
	toStr := r.URL.Query().Get("to")

	var sessions []store.Session
	var total int
	var err error

	if fromStr != "" && toStr != "" {
		from, _ := time.Parse(time.RFC3339, fromStr)
		to, _ := time.Parse(time.RFC3339, toStr)
		if !from.IsZero() && !to.IsZero() {
			sessions, err = s.store.SessionsByDateRange(from, to, limit, offset)
			if err == nil {
				total, _ = s.store.SessionCount(&from, &to)
			}
		}
	}
	if sessions == nil {
		sessions, err = s.store.SessionHistory(limit, offset)
		if err == nil {
			total, _ = s.store.SessionCount(nil, nil)
		}
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.store.EnrichWithOdometer(sessions)
	writeJSON(w, map[string]any{"sessions": sessions, "total": total})
}

func (s *Server) handleCostLog(w http.ResponseWriter, r *http.Request) {
	fromStr := r.URL.Query().Get("from")
	toStr := r.URL.Query().Get("to")
	limit := queryInt(r, "limit", 50)
	offset := queryInt(r, "offset", 0)

	if fromStr == "" || toStr == "" {
		http.Error(w, "from and to parameters required", http.StatusBadRequest)
		return
	}

	from, err1 := time.Parse("2006-01-02", fromStr)
	to, err2 := time.Parse("2006-01-02", toStr)
	if err1 != nil || err2 != nil {
		http.Error(w, "invalid date format (expected YYYY-MM-DD)", http.StatusBadRequest)
		return
	}
	to = to.Add(24 * time.Hour)

	items, total, err := s.store.CostLogItems(from, to, limit, offset)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"items": items, "total": total})
}

func (s *Server) handleSessionReport(w http.ResponseWriter, r *http.Request) {
	fromStr := r.URL.Query().Get("from")
	toStr := r.URL.Query().Get("to")

	if fromStr == "" || toStr == "" {
		http.Error(w, "from and to parameters required", http.StatusBadRequest)
		return
	}

	from, err1 := time.Parse(time.RFC3339, fromStr)
	to, err2 := time.Parse(time.RFC3339, toStr)
	if err1 != nil || err2 != nil {
		// Try date-only format
		from, err1 = time.Parse("2006-01-02", fromStr)
		to, err2 = time.Parse("2006-01-02", toStr)
		if err1 != nil || err2 != nil {
			http.Error(w, "invalid date format", http.StatusBadRequest)
			return
		}
		to = to.Add(24 * time.Hour) // make "to" exclusive end-of-day
	}

	report, err := s.store.SessionReportByRange(from, to)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, report)
}

func (s *Server) handleSessionReportHTML(w http.ResponseWriter, r *http.Request) {
	fromStr := r.URL.Query().Get("from")
	toStr := r.URL.Query().Get("to")

	if fromStr == "" || toStr == "" {
		http.Error(w, "from and to parameters required", http.StatusBadRequest)
		return
	}

	from, err1 := time.Parse(time.RFC3339, fromStr)
	to, err2 := time.Parse(time.RFC3339, toStr)
	if err1 != nil || err2 != nil {
		from, err1 = time.Parse("2006-01-02", fromStr)
		to, err2 = time.Parse("2006-01-02", toStr)
		if err1 != nil || err2 != nil {
			http.Error(w, "invalid date format", http.StatusBadRequest)
			return
		}
		to = to.Add(24 * time.Hour)
	}

	report, err := s.store.SessionReportByRange(from, to)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	logItems, _, _ := s.store.CostLogItems(from, to, 10000, 0)

	// Load timezone for time formatting
	tzName := s.store.GetDefault("display.timezone", "UTC")
	loc, _ := time.LoadLocation(tzName)
	if loc == nil {
		loc = time.UTC
	}
	formatCostLogDescriptions(logItems, loc)

	fromDisplay := from.Format("2006-01-02")
	toDisplay := to.Add(-24 * time.Hour).Format("2006-01-02")
	if fromDisplay == toDisplay {
		toDisplay = fromDisplay
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Charging Report %s — %s</title>
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; color: #1a1a2e; background: #f8f9fa; padding: 2rem; max-width: 900px; margin: 0 auto; }
  h1 { font-size: 1.5rem; margin-bottom: 0.25rem; }
  .subtitle { color: #6c757d; margin-bottom: 2rem; font-size: 0.95rem; }
  .grid { display: grid; grid-template-columns: repeat(4, 1fr); gap: 1rem; margin-bottom: 2rem; }
  .stat { background: #fff; border: 1px solid #dee2e6; border-radius: 8px; padding: 1rem; text-align: center; }
  .stat.highlight { background: #e8f5e9; border-color: #a5d6a7; }
  .stat-value { font-size: 1.4rem; font-weight: 700; color: #1a1a2e; }
  .stat-label { font-size: 0.75rem; color: #6c757d; margin-top: 0.25rem; }
  h2 { font-size: 1.1rem; margin: 1.5rem 0 0.75rem; }
  table { width: 100%%; border-collapse: collapse; background: #fff; border: 1px solid #dee2e6; border-radius: 8px; overflow: hidden; margin-bottom: 1.5rem; }
  th { background: #f1f3f5; font-weight: 600; font-size: 0.8rem; text-transform: uppercase; color: #495057; }
  th, td { padding: 0.6rem 0.75rem; text-align: left; border-bottom: 1px solid #f1f3f5; font-size: 0.85rem; }
  tr:last-child td { border-bottom: none; }
  td.num { text-align: right; font-variant-numeric: tabular-nums; }
  .footer { color: #adb5bd; font-size: 0.75rem; margin-top: 2rem; text-align: center; }
  @media print { body { padding: 0; } .grid { gap: 0.5rem; } }
  @media (max-width: 600px) { .grid { grid-template-columns: repeat(2, 1fr); } }
</style>
</head>
<body>
<h1>Charging Report</h1>
<p class="subtitle">%s — %s</p>
`, fromDisplay, toDisplay, fromDisplay, toDisplay)

	// Report summary grid
	distStr := "-"
	costPer100 := "-"
	whPerKm := "-"
	if report.Distance > 0 {
		distStr = fmt.Sprintf("%.0f", report.Distance)
		costPer100 = fmt.Sprintf("%.2f", report.CostPer100km)
		whPerKm = fmt.Sprintf("%.0f", report.WhPerKm)
	}

	fmt.Fprintf(w, `<div class="grid">
  <div class="stat"><div class="stat-value">%d</div><div class="stat-label">Sessions</div></div>
  <div class="stat"><div class="stat-value">%.1f</div><div class="stat-label">kWh charged</div></div>
  <div class="stat"><div class="stat-value">%.2f</div><div class="stat-label">PLN charging</div></div>
  <div class="stat"><div class="stat-value">%.3f</div><div class="stat-label">PLN/kWh avg</div></div>
  <div class="stat"><div class="stat-value">%.1f</div><div class="stat-label">Hours charging</div></div>
  <div class="stat"><div class="stat-value">%.2f</div><div class="stat-label">kWh idle</div></div>
  <div class="stat"><div class="stat-value">%.2f</div><div class="stat-label">PLN idle</div></div>
  <div class="stat"><div class="stat-value">%.2f</div><div class="stat-label">PLN external</div></div>
  <div class="stat highlight"><div class="stat-value">%.2f</div><div class="stat-label">PLN total</div></div>
  <div class="stat"><div class="stat-value">%s</div><div class="stat-label">km driven</div></div>
  <div class="stat highlight"><div class="stat-value">%s</div><div class="stat-label">PLN/100km</div></div>
  <div class="stat"><div class="stat-value">%s</div><div class="stat-label">Wh/km</div></div>
</div>
`,
		report.TotalSessions, report.TotalEnergy, report.TotalCost, report.AvgCostPerKWh,
		report.TotalDuration, report.IdleEnergy, report.IdleCost, report.ExternalCosts,
		report.GrandTotalCost, distStr, costPer100, whPerKm)

	// Cost log table
	if len(logItems) > 0 {
		fmt.Fprint(w, `<h2>Cost Log</h2>
<table>
<tr><th>Date</th><th>Type</th><th>Description</th><th>Energy</th><th>Cost</th></tr>
`)
		for _, item := range logItems {
			energy := "-"
			if item.EnergyKWh > 0 {
				energy = fmt.Sprintf("%.2f kWh", item.EnergyKWh)
			}
			cost := fmt.Sprintf("%.2f PLN", item.Cost)
			desc := item.Description
			if item.Type == "charging" && item.Distance > 0 {
				desc += fmt.Sprintf(" (%.0f km, %.1f kWh/100km)", item.Distance, item.KWhPer100km)
			}
			fmt.Fprintf(w, "<tr><td>%s</td><td>%s</td><td>%s</td><td class=\"num\">%s</td><td class=\"num\">%s</td></tr>\n",
				item.Date, item.Type, desc, energy, cost)
		}
		fmt.Fprint(w, "</table>\n")
	}

	fmt.Fprintf(w, `<div class="footer">Generated %s — grosz EV Charging Manager</div>
</body>
</html>`, time.Now().Format("2006-01-02 15:04"))
}

func (s *Server) handleExternalCosts(w http.ResponseWriter, r *http.Request) {
	fromStr := r.URL.Query().Get("from")
	toStr := r.URL.Query().Get("to")
	if fromStr == "" || toStr == "" {
		http.Error(w, "from and to parameters required", http.StatusBadRequest)
		return
	}
	from, err1 := time.Parse("2006-01-02", fromStr)
	to, err2 := time.Parse("2006-01-02", toStr)
	if err1 != nil || err2 != nil {
		http.Error(w, "invalid date format", http.StatusBadRequest)
		return
	}
	to = to.Add(24 * time.Hour)

	costs, err := s.store.ExternalCostsByDateRange(from, to)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"costs": costs})
}

func (s *Server) handleAddExternalCost(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Date        string  `json:"date"`
		Description string  `json:"description"`
		Amount      float64 `json:"amount"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Date == "" || req.Description == "" || req.Amount == 0 {
		http.Error(w, "date, description, and non-zero amount required", http.StatusBadRequest)
		return
	}

	id, err := s.store.AddExternalCost(store.ExternalCost{
		Date:        req.Date,
		Description: req.Description,
		Amount:      req.Amount,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "id": id})
}

func (s *Server) handleDeleteExternalCost(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := s.store.DeleteExternalCost(id); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// formatCostLogDescriptions sets the Description field for charging items
// using the given timezone location.
func formatCostLogDescriptions(items []store.CostLogItem, loc *time.Location) {
	for i := range items {
		if items[i].Type != "charging" {
			continue
		}
		start, err := time.Parse(time.RFC3339, items[i].StartTime)
		if err != nil {
			continue
		}
		startStr := start.In(loc).Format("15:04")
		endStr := "…"
		if items[i].StopTime != "" {
			if stop, err := time.Parse(time.RFC3339, items[i].StopTime); err == nil {
				endStr = stop.In(loc).Format("15:04")
			}
		}
		items[i].Description = fmt.Sprintf("Charging %s–%s", startStr, endStr)
	}
}

func queryInt(r *http.Request, key string, def int) int {
	s := r.URL.Query().Get(key)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
