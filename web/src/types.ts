export interface Measurement {
  value: number;
  unit: string;
  timestamp: string;
}

export interface ConnectorInfo {
  id: number;
  status: string;
  transactionId?: number;
  measurements?: Record<string, Measurement>;
}

export interface ChargePointInfo {
  id: string;
  connected: boolean;
  connectedAt?: string;
  vendor?: string;
  model?: string;
  serial?: string;
  firmware?: string;
  connectors: ConnectorInfo[];
}

export interface SchedulePeriod {
  start: string;
  end: string;
  power: number;
  price: number;
  source?: 'auto' | 'user_force';
}

export interface ScheduleSlot {
  date: string;
  deadline: string;
  periods: SchedulePeriod[];
  cost: number;
  energy: number;
  cancelled: boolean;
}

export interface Schedule {
  slots: ScheduleSlot[];
  cost: number;
  energy: number;
  deadline: string;
}

export interface ScheduleOverride {
  id: number;
  kind: 'force' | 'block';
  start: string;
  end: string;
  powerW: number;
  createdAt: string;
}

export interface StatusResponse {
  chargePoints: ChargePointInfo[];
  schedule?: Schedule;
  overrides?: ScheduleOverride[];
  charging: boolean;
  mode: 'off' | 'schedule' | 'force';
  soc: number;
  minSoc: number;
  skipAboveSoc: number;
  skipReason?: string;
  skipReasonKey?: string;
  skipReasonParams?: Record<string, string>;
  deadlineTime: string;
  batteryAutonomy: number;
  chargingStatus: number;
  plugStatus: number;
  chargingRemainingTime: number;
  batteryTimestamp?: string;
  vehicleModel?: string;
  vehiclePicture?: string;
  mileage?: number;
}

export interface MeterLive {
  totalPower: number;
  frequency: number;
  phases: { power: number; current: number; voltage: number }[];
  timestamp: string;
}

export interface Rate {
  start: string;
  end: string;
  price: number;
}

export interface OcppEvent {
  id: number;
  timestamp: string;
  direction: string;
  chargeBox: string;
  action: string;
  payload: unknown;
}

export interface HourlyEnergy {
  hour: string;
  energyWh: number;
  powerW: number;
}

export interface Session {
  id: number;
  chargeBox: string;
  connectorId: number;
  transactionId: number;
  idTag?: string;
  startTime: string;
  stopTime?: string;
  meterStart: number;
  meterStop?: number;
  energy: number;
  cost?: number;
  status: string;
}
