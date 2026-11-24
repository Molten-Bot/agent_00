import React, { useState } from "react";
import { Timer, Clock3, Sun, CalendarDays, CalendarRange } from "lucide-react";

const units = [
  { label: "min", name: "minutes", hours: 1 / 60, Icon: Timer },
  { label: "hour", name: "hours", hours: 1, Icon: Clock3 },
  { label: "day", name: "days", hours: 24, Icon: Sun },
  { label: "week", name: "weeks", hours: 168, Icon: CalendarDays },
  { label: "month", name: "months", hours: 720, Icon: CalendarRange },
];

function initialUnit(hours) {
  for (let i = units.length - 1; i > 0; i--) {
    if (hours >= units[i].hours && Number.isInteger(hours / units[i].hours)) return i;
  }
  return 0;
}

export function ScheduleInterval({ hours, onChange }) {
  const [index, setIndex] = useState(() => initialUnit(hours));
  const unit = units[index];
  const amount = Math.round((hours / unit.hours) * 1e8) / 1e8;
  const maximum = Math.floor(8760 / unit.hours);
  function selectUnit(next) {
    if (next === index) return;
    setIndex(next);
    onChange(Math.min(Math.max(1, Math.round(amount)), Math.floor(8760 / units[next].hours)) * units[next].hours);
  }
  return (
    <div className="wf-schedule-interval">
      <span className="wf-interval-label">Repeat every</span>
      <div className="wf-interval-units" role="group" aria-label="Repeat interval unit">
        {units.map(({ label, name, Icon }, i) => (
          <button type="button" key={label} aria-label={name} aria-pressed={index === i} onClick={() => selectUnit(i)}>
            <Icon size={16} aria-hidden="true" />
            <span>{label}</span>
          </button>
        ))}
      </div>
      <input className="wf-interval-scale" type="range" min="0" max="4" step="1" value={index}
        aria-label="Repeat interval scale" aria-valuetext={unit.name}
        onChange={(e) => selectUnit(Number(e.target.value))} />
      <label className="wf-field wf-interval-amount">
        <span>Repeat every ({unit.name})</span>
        <input type="number" min="1" max={maximum} step="1" required value={amount || ""}
          onChange={(e) => onChange(Number(e.target.value) * unit.hours)} />
      </label>
      {index === 4 && <p className="wf-interval-hint">One month is a fixed 30-day interval.</p>}
    </div>
  );
}
