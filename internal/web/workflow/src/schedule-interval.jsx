import React, { useId, useState } from "react";
import { Timer, Clock3, Sun, CalendarDays, CalendarRange } from "lucide-react";

const units = [
  { label: "min", name: "minutes", hours: 1 / 60, min: 5, max: 60, Icon: Timer },
  { label: "hour", name: "hours", hours: 1, min: 1, max: 24, Icon: Clock3 },
  { label: "day", name: "days", hours: 24, min: 1, max: 7, Icon: Sun },
  { label: "week", name: "weeks", hours: 168, min: 1, max: 4, Icon: CalendarDays },
  { label: "month", name: "months", hours: 720, min: 1, max: 12, Icon: CalendarRange },
];

function sliderAmount(amount, { min, max }) {
  return Math.min(Math.max(min, Math.round(amount)), max);
}

function initialUnit(hours) {
  for (let i = units.length - 1; i > 0; i--) {
    if (hours >= units[i].hours && Number.isInteger(hours / units[i].hours)) return i;
  }
  return 0;
}

export function ScheduleInterval({ hours, onChange }) {
  const id = useId();
  const [index, setIndex] = useState(() => initialUnit(hours));
  const unit = units[index];
  const amount = Math.round((hours / unit.hours) * 1e8) / 1e8;
  const sliderValue = sliderAmount(amount, unit);
  const savedValue = amount !== sliderValue;
  function selectUnit(next) {
    if (next === index) return;
    setIndex(next);
    onChange(sliderAmount(amount, units[next]) * units[next].hours);
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
      <div className="wf-interval-amount">
        <label htmlFor={id}>Repeat every ({unit.name})</label>
        <span>{amount}{savedValue && " (saved)"}</span>
        <input id={id} type="range" min={unit.min} max={unit.max} step="1" value={sliderValue}
          aria-valuetext={savedValue ? `${sliderValue} ${unit.name} (${amount} saved)` : `${amount}`}
          // Choosing the current thumb position does not fire onChange.
          onClick={() => {
            if (savedValue) onChange(sliderValue * unit.hours);
          }}
          onKeyDown={(e) => {
            if (!savedValue) return;
            if ((e.key === "Home" && sliderValue === unit.min) || (e.key === "End" && sliderValue === unit.max)) {
              onChange(sliderValue * unit.hours);
            }
          }}
          onChange={(e) => onChange(Number(e.target.value) * unit.hours)} />
      </div>
      {index === 4 && <p className="wf-interval-hint">One month is a fixed 30-day interval.</p>}
    </div>
  );
}
