package events

const (
	SourceScheduler Source = "scheduler"
	SourceTariff    Source = "tariff"
	// SourcePstryk covers Pstryk.pl API consumption fetches. Tariff pricing
	// fetches stay on SourceTariff so existing event-log filters keep working.
	SourcePstryk  Source = "pstryk"
	SourceRenault Source = "renault"
	SourceMeter   Source = "meter"
	SourceZappi   Source = "zappi"
	SourceOCPP    Source = "ocpp"
	SourceAuth    Source = "auth"
	SourceCosts   Source = "costs"
	SourceStore   Source = "store"
	SourceWeb     Source = "web"
	SourceApp     Source = "app"
)
