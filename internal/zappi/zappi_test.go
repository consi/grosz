package zappi

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsZappi(t *testing.T) {
	tests := []struct {
		vendor, model string
		want          bool
	}{
		{"Myenergi", "Zappi", true},
		{"myenergi", "zappi", true},
		{"MYENERGI", "ZAPPI", true},
		{"Myenergi", "Zappi2", false},
		{"Other", "Zappi", false},
		{"Myenergi", "Eddi", false},
		{"", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.vendor+"/"+tt.model, func(t *testing.T) {
			assert.Equal(t, tt.want, IsZappi(tt.vendor, tt.model))
		})
	}
}
