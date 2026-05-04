package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChargeBoxID_PrefersChargerKey(t *testing.T) {
	s := testStore(t)
	require.NoError(t, s.Set("charger.charge_box_id", "CB_NEW"))
	require.NoError(t, s.Set("zappi.charge_box_id", "CB_LEGACY"))

	require.Equal(t, "CB_NEW", s.ChargeBoxID())
}

func TestChargeBoxID_FallsBackToZappi(t *testing.T) {
	s := testStore(t)
	require.NoError(t, s.Set("zappi.charge_box_id", "CB_LEGACY"))

	require.Equal(t, "CB_LEGACY", s.ChargeBoxID())
}

func TestChargeBoxID_FallsBackWhenChargerKeyEmpty(t *testing.T) {
	s := testStore(t)
	require.NoError(t, s.Set("charger.charge_box_id", ""))
	require.NoError(t, s.Set("zappi.charge_box_id", "CB_LEGACY"))

	require.Equal(t, "CB_LEGACY", s.ChargeBoxID())
}

func TestChargeBoxID_EmptyWhenNothingSet(t *testing.T) {
	s := testStore(t)

	require.Equal(t, "", s.ChargeBoxID())
}

func TestChargerIDTag_PrefersChargerKey(t *testing.T) {
	s := testStore(t)
	require.NoError(t, s.Set("charger.id_tag", "tag_new"))
	require.NoError(t, s.Set("zappi.id_tag", "tag_legacy"))

	require.Equal(t, "tag_new", s.ChargerIDTag())
}

func TestChargerIDTag_FallsBackToZappi(t *testing.T) {
	s := testStore(t)
	require.NoError(t, s.Set("zappi.id_tag", "tag_legacy"))

	require.Equal(t, "tag_legacy", s.ChargerIDTag())
}

func TestChargerIDTag_FallsBackWhenChargerKeyEmpty(t *testing.T) {
	s := testStore(t)
	require.NoError(t, s.Set("charger.id_tag", ""))
	require.NoError(t, s.Set("zappi.id_tag", "tag_legacy"))

	require.Equal(t, "tag_legacy", s.ChargerIDTag())
}

func TestChargerIDTag_DefaultWhenNothingSet(t *testing.T) {
	s := testStore(t)

	require.Equal(t, defaultIDTag, s.ChargerIDTag())
}
