package tariff

import (
	"testing"
	"time"
)

func TestDropMissingPrices(t *testing.T) {
	base := time.Date(2026, 11, 17, 22, 0, 0, 0, time.UTC)
	got := dropMissingPrices([]Rate{
		{Start: base, End: base.Add(time.Hour), Price: 1.0087},
		{Start: base.Add(time.Hour), End: base.Add(2 * time.Hour), Price: 0}, // unpublished hour
		{Start: base.Add(2 * time.Hour), End: base.Add(3 * time.Hour), Price: -0.05},
	})
	if len(got) != 2 || got[0].Price != 1.0087 || got[1].Price != -0.05 {
		t.Fatalf("unexpected: %+v", got)
	}
}
