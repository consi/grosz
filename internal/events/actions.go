package events

// Action constants. Add new entries here rather than passing literal
// strings to a recorder. Naming convention: camelCase verbNoun
// (e.g., loadSchedule, transactionStarted).

// scheduler
const (
	ActionLoadSchedule        Action = "loadSchedule"
	ActionSaveSchedule        Action = "saveSchedule"
	ActionRecompute           Action = "recompute"
	ActionApplyProfile        Action = "applyProfile"
	ActionControlCharging     Action = "controlCharging"
	ActionModeChangeRequested Action = "modeChangeRequested"
	ActionModeChange          Action = "modeChange"
	ActionMissedPeriod        Action = "missedPeriod"
	ActionCancelSlot          Action = "cancelSlot"
	ActionClearSchedule       Action = "clearSchedule"
	ActionReloadConfig        Action = "reloadConfig"
	ActionRestoreSlot         Action = "restoreSlot"
	ActionSetConfig           Action = "setConfig"
	ActionUpdateSoC           Action = "updateSoC"
	ActionCreateOverride      Action = "createOverride"
	ActionDeleteOverride      Action = "deleteOverride"
)

// meter
const (
	ActionMeterPoll          Action = "poll"
	ActionMeterResetDetected Action = "meterResetDetected"
)

// ocpp
const (
	ActionFinalizeSessionOnStop  Action = "finalizeSessionOnStop"
	ActionChargerConnected       Action = "chargerConnected"
	ActionChargerDisconnected    Action = "chargerDisconnected"
	ActionFirmwareStatus         Action = "firmwareStatus"
	ActionDiagnosticsStatus      Action = "diagnosticsStatus"
	ActionTransactionStarted     Action = "transactionStarted"
	ActionTransactionStopped     Action = "transactionStopped"
	ActionConnectorStatusChanged Action = "connectorStatusChanged"
	ActionRemoteStartRequested   Action = "remoteStartRequested"
	ActionRemoteStopRequested    Action = "remoteStopRequested"
	ActionChargingProfileApplied Action = "chargingProfileApplied"
	ActionChargingProfileCleared Action = "chargingProfileCleared"
	ActionResetRequested         Action = "resetRequested"
	ActionFirmwareUpdateRequested Action = "firmwareUpdateRequested"
	ActionChargerBoot            Action = "chargerBoot"
	ActionStatusCheckTriggered   Action = "statusCheckTriggered"
)

// tariff
const (
	ActionFetchRates         Action = "fetchRates"
	ActionFilterPlaceholders Action = "filterPlaceholders"
)

// renault
const (
	ActionRenaultPoll      Action = "poll"
	ActionVehicleDetails   Action = "vehicleDetails"
)

// auth
const (
	ActionLogin                 Action = "login"
	ActionLogout                Action = "logout"
	ActionCredentialRegistered  Action = "credentialRegistered"
	ActionCredentialDeleted     Action = "credentialDeleted"
)

// costs
const (
	ActionCostsAdd    Action = "add"
	ActionCostsDelete Action = "delete"
)

// store
const (
	ActionIdleSnapshotDaily   Action = "idleSnapshotDaily"
	ActionPurgeSystemEvents   Action = "purgeSystemEvents"
	ActionPurgeOcppEvents     Action = "purgeOcppEvents"
	ActionPurgeChartMarkers   Action = "purgeChartMarkers"
	ActionPurgeMeterReadings  Action = "purgeMeterReadings"
	ActionPurgePhaseReadings  Action = "purgePhaseReadings"
	ActionPurgeOverrides      Action = "purgeOverrides"
	ActionPurgeRates          Action = "purgeRates"
	ActionPurgeWebSessions    Action = "purgeWebSessions"
)

// zappi
const (
	ActionZappiSetup Action = "setup"
)

// web (user-initiated runtime actions; settings changes; manual controls)
const (
	ActionSettingsUpdated     Action = "settingsUpdated"
	ActionManualChargeStarted Action = "manualChargeStarted"
	ActionManualChargeStopped Action = "manualChargeStopped"
	ActionModeSwitchRequested Action = "modeSwitchRequested"
)

// app
const (
	ActionAppStarted  Action = "appStarted"
	ActionAppShutdown Action = "appShutdown"
)
