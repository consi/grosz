package store

// defaultIDTag is used when neither charger.id_tag nor zappi.id_tag is set.
// Matches the historic seed value from cmd/grosz so existing behavior is unchanged.
const defaultIDTag = "grosz"

// ChargeBoxID returns the configured OCPP charge-box ID. It prefers the
// vendor-neutral charger.charge_box_id setting and falls back to the legacy
// zappi.charge_box_id for upgrade compatibility.
func (s *Store) ChargeBoxID() string {
	if v := s.GetDefault("charger.charge_box_id", ""); v != "" {
		return v
	}
	return s.GetDefault("zappi.charge_box_id", "")
}

// ChargerIDTag returns the OCPP idTag used for RemoteStartTransaction.
// Prefers charger.id_tag, falls back to zappi.id_tag, finally defaultIDTag.
func (s *Store) ChargerIDTag() string {
	if v := s.GetDefault("charger.id_tag", ""); v != "" {
		return v
	}
	return s.GetDefault("zappi.id_tag", defaultIDTag)
}
