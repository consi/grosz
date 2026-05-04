package tariff

import (
	"time"
)

// Rate represents an electricity price for a time period.
type Rate struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	Price float64   `json:"price"` // PLN/kWh gross
}

// Provider fetches and caches electricity tariff rates.
type Provider interface {
	// Rates returns currently known price rates, sorted by Start ascending.
	Rates() ([]Rate, error)
	// Name returns the provider identifier.
	Name() string
	// Stop shuts down the background refresh loop.
	Stop()
}
