import React, { useId } from "react";
import { MessageSquareCode, CircleOff } from "lucide-react";

const passes = [0, 1, 2, 4, 8];

export function ReviewPasses({ value, onChange }) {
  const id = useId();
  const savedValue = !passes.includes(value);
  const index = passes.reduce((best, count, i) =>
    Math.abs(count - value) < Math.abs(passes[best] - value) ? i : best, 0);
  const label = value === 0 ? "Off · 0 passes" : `${value} ${value === 1 ? "pass" : "passes"}`;
  const Icon = value === 0 ? CircleOff : MessageSquareCode;
  return (
    <div className="wf-review-slider">
      <label htmlFor={id}>Review passes</label>
      <output htmlFor={id} className="wf-review-value">
        <Icon size={22} aria-hidden="true" />
        <span>{label}</span>
      </output>
      <input id={id} type="range" min="0" max="4" step="1" value={index}
        aria-valuetext={savedValue ? `${label}, saved value; choose a preset to change` : label}
        aria-describedby={savedValue ? `${id}-saved` : undefined}
        onChange={(event) => onChange(passes[Number(event.target.value)])} />
      <div className="wf-review-stops" role="group" aria-label="Review pass presets">
        {passes.map((count) => (
          <button type="button" key={count} aria-label={`${count} ${count === 1 ? "pass" : "passes"}`}
            aria-pressed={value === count} onClick={() => onChange(count)}>
            <span>{count}</span>
            <small>{count === 0 ? "Off" : count === 1 ? "pass" : "passes"}</small>
          </button>
        ))}
      </div>
      {savedValue && <p id={`${id}-saved`} className="wf-muted">Saved value: {value} passes. Choose a preset to change it.</p>}
    </div>
  );
}
