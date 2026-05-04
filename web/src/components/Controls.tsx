import type { ChargePointInfo } from '../types';

interface Props {
  chargePoints: ChargePointInfo[];
  onStart: (cpId: string) => void;
  onStop: (cpId: string) => void;
}

export function Controls({ chargePoints, onStart, onStop }: Props) {
  const connected = chargePoints?.filter((cp) => cp.connected) || [];
  if (!connected.length) return null;

  return (
    <div className="card">
      <h2>Manual Control</h2>
      {connected.map((cp) => {
        const hasTransaction = cp.connectors?.some((c) => c.transactionId);
        return (
          <div key={cp.id} className="control-row">
            <span>{cp.id}</span>
            {hasTransaction ? (
              <button className="btn danger" onClick={() => onStop(cp.id)}>
                Stop Charging
              </button>
            ) : (
              <button className="btn primary" onClick={() => onStart(cp.id)}>
                Start Charging
              </button>
            )}
          </div>
        );
      })}
    </div>
  );
}
