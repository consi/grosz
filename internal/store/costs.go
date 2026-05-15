package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/consi/grosz/internal/events"
)

// ExternalCost represents a manually added cost entry.
type ExternalCost struct {
	ID          int64   `json:"id"`
	Date        string  `json:"date"`
	Description string  `json:"description"`
	Amount      float64 `json:"amount"`
	CreatedAt   string  `json:"createdAt"`
}

// AddExternalCost inserts a new external cost entry and returns its ID.
func (s *Store) AddExternalCost(cost ExternalCost) (int64, error) {
	result, err := s.db.Exec(
		`INSERT INTO external_costs (date, description, amount) VALUES (?, ?, ?)`,
		cost.Date, cost.Description, cost.Amount,
	)
	if err != nil {
		return 0, fmt.Errorf("insert external cost: %w", err)
	}
	id, _ := result.LastInsertId()

	events.Info(s, events.SourceCosts, events.ActionCostsAdd,
		map[string]any{"date": cost.Date, "description": cost.Description, "amount": cost.Amount},
		map[string]any{"id": id},
	)

	return id, nil
}

// DeleteExternalCost removes an external cost by ID.
func (s *Store) DeleteExternalCost(id int64) error {
	result, err := s.db.Exec("DELETE FROM external_costs WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete external cost: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}

	events.Info(s, events.SourceCosts, events.ActionCostsDelete,
		map[string]any{"id": id},
		map[string]any{"deleted": true},
	)

	return nil
}

// ExternalCostsByDateRange returns external costs within a date range, newest first.
func (s *Store) ExternalCostsByDateRange(from, to time.Time) ([]ExternalCost, error) {
	rows, err := s.db.Query(
		`SELECT id, date, description, amount, created_at FROM external_costs
		 WHERE date >= ? AND date < ?
		 ORDER BY date DESC, id DESC`,
		from.Format("2006-01-02"),
		to.Format("2006-01-02"),
	)
	if err != nil {
		return nil, fmt.Errorf("query external costs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	costs := make([]ExternalCost, 0)
	for rows.Next() {
		var c ExternalCost
		if err := rows.Scan(&c.ID, &c.Date, &c.Description, &c.Amount, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan external cost: %w", err)
		}
		costs = append(costs, c)
	}
	return costs, rows.Err()
}

// ExternalCostSumByDateRange returns the total of external costs in a date range.
func (s *Store) ExternalCostSumByDateRange(from, to time.Time) (float64, error) {
	var sum float64
	err := s.db.QueryRow(
		`SELECT COALESCE(SUM(amount), 0) FROM external_costs
		 WHERE date >= ? AND date < ?`,
		from.Format("2006-01-02"),
		to.Format("2006-01-02"),
	).Scan(&sum)
	if err != nil {
		return 0, fmt.Errorf("sum external costs: %w", err)
	}
	return sum, nil
}
