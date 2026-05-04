package store

import (
	"fmt"
	"time"
)

// CostLogItem represents a single entry in the unified cost log.
type CostLogItem struct {
	Type        string  `json:"type"`                  // "charging", "external", "idle"
	Date        string  `json:"date"`                  // "2006-01-02"
	Description string  `json:"description"`           // "Grid fee", "Idle consumption"
	EnergyKWh   float64 `json:"energyKwh,omitempty"`
	Cost        float64 `json:"cost"`
	SourceID    int64   `json:"sourceId,omitempty"`    // ID in source table
	StartTime   string  `json:"startTime,omitempty"`   // RFC3339 (charging only)
	StopTime    string  `json:"stopTime,omitempty"`    // RFC3339 (charging only)
	Distance    float64 `json:"distance,omitempty"`    // km (charging only)
	KWhPer100km float64 `json:"kwhPer100km,omitempty"` // consumption (charging only)
}

const costLogQuery = `
WITH items AS (
    SELECT 'charging' AS type,
           date(start_time) AS date,
           start_time AS sort_key,
           '' AS description,
           energy AS energy_kwh, cost, id AS source_id,
           start_time, COALESCE(stop_time, '') AS stop_time
    FROM charging_sessions
    WHERE start_time >= ? AND start_time < ? AND status = 'completed'
    UNION ALL
    SELECT 'idle', date, date || 'T23:59:59Z',
           'Idle consumption', energy_kwh, cost, id,
           '', ''
    FROM daily_idle
    WHERE date >= ? AND date < ?
    UNION ALL
    SELECT 'external', date, date || 'T12:00:00Z',
           description, 0, amount, id,
           '', ''
    FROM external_costs
    WHERE date >= ? AND date < ?
)
`

// CostLogItems returns a paginated unified cost log across all cost sources.
func (s *Store) CostLogItems(from, to time.Time, limit, offset int) ([]CostLogItem, int, error) {
	fromDate := from.Format("2006-01-02")
	toDate := to.Format("2006-01-02")
	fromRFC := from.UTC().Format(time.RFC3339)
	toRFC := to.UTC().Format(time.RFC3339)

	// Count total
	var total int
	err := s.db.QueryRow(
		costLogQuery+`SELECT COUNT(*) FROM items`,
		fromRFC, toRFC, fromDate, toDate, fromDate, toDate,
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("cost log count: %w", err)
	}

	// Fetch page
	rows, err := s.db.Query(
		costLogQuery+`SELECT type, date, description, energy_kwh, cost, source_id, start_time, stop_time
		FROM items ORDER BY date DESC, sort_key DESC
		LIMIT ? OFFSET ?`,
		fromRFC, toRFC, fromDate, toDate, fromDate, toDate, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("cost log query: %w", err)
	}
	defer rows.Close()

	var items []CostLogItem
	for rows.Next() {
		var item CostLogItem
		if err := rows.Scan(&item.Type, &item.Date, &item.Description, &item.EnergyKWh, &item.Cost, &item.SourceID, &item.StartTime, &item.StopTime); err != nil {
			return nil, 0, fmt.Errorf("scan cost log: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("cost log rows: %w", err)
	}

	// Enrich charging items with odometer data
	s.enrichCostLogWithOdometer(items, from, to)

	return items, total, nil
}

// enrichCostLogWithOdometer adds distance and kWh/100km to charging items.
func (s *Store) enrichCostLogWithOdometer(items []CostLogItem, from, to time.Time) {
	var hasCharging bool
	for _, item := range items {
		if item.Type == "charging" {
			hasCharging = true
			break
		}
	}
	if !hasCharging {
		return
	}

	// Fetch the full sessions for this date range (needed for consecutive-session odometer logic)
	sessions, err := s.SessionsByDateRange(from, to, 1000, 0)
	if err != nil {
		return
	}
	s.EnrichWithOdometer(sessions)

	// Build lookup: session ID → enriched session
	lookup := make(map[int64]*Session, len(sessions))
	for i := range sessions {
		lookup[sessions[i].ID] = &sessions[i]
	}

	// Apply to cost log items
	for i := range items {
		if items[i].Type != "charging" {
			continue
		}
		if sess, ok := lookup[items[i].SourceID]; ok {
			items[i].Distance = sess.Distance
			items[i].KWhPer100km = sess.KWhPer100km
		}
	}
}
