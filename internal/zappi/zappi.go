package zappi

import "strings"

const (
	VendorMyenergi = "Myenergi"
	ModelZappi     = "Zappi"

	// OCPP configuration keys specific to Zappi
	KeyCommercialMode          = "CommercialMode"
	KeyFreeCharging            = "FreeCharging"
	KeyRandomSmartChargeDelay  = "RandomSmartChargeDelay"
	KeyMeterValuesSampledData  = "MeterValuesSampledData"
	KeyMeterValueSampleInterval = "MeterValueSampleInterval"
	KeyMaintenanceMode         = "MaintenanceMode"
	KeyFreeVendScreen          = "FreeVendScreen"
	KeyPlugAndChargeId         = "PlugAndChargeId"
	KeyStopTransactionOnInvalidId   = "StopTransactionOnInvalidId"
	KeyAuthorizeRemoteTxRequests    = "AuthorizeRemoteTxRequests"
	KeyChargerName             = "ChargerName"
	KeyPaymentURL              = "PaymentURL"

	DefaultMeasurands = "Energy.Active.Import.Register,Power.Active.Import,Current.Import,Current.Offered,Voltage"
)

// IsZappi returns true if the vendor and model identify a MyEnergi Zappi charger.
func IsZappi(vendor, model string) bool {
	return strings.EqualFold(vendor, VendorMyenergi) && strings.EqualFold(model, ModelZappi)
}
