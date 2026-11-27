import React, { useId } from "react";
import { Signal, SignalHigh, SignalLow, SignalMedium, SignalZero } from "lucide-react";

const presets = [
  { count: 0, Icon: SignalZero },
  { count: 1, Icon: SignalLow },
  { count: 2, Icon: SignalMedium },
  { count: 4, Icon: SignalHigh },
  { count: 8, Icon: Signal },
  { count: 10, Icon: Signal },
];

export function ReviewPasses({ value, onChange }) {
  const id = useId();
  const index = presets.reduce((best, { count }, i) =>
    Math.abs(count - value) < Math.abs(presets[best].count - value) ? i : best, 0);
  const savedValue = presets[index].count !== value;
  const label = value === 0 ? "Inherit default" : `${value} ${value === 1 ? "pass" : "passes"}`;
  return (
    <div className="wf-review-slider">
      <label htmlFor={id}>Review passes</label>
      <input id={id} type="range" min="0" max={presets.length - 1} step="1" value={index} title={label}
        aria-valuetext={savedValue ? `${label}, saved value; choose a preset to change` : label}
        onChange={(event) => onChange(presets[Number(event.target.value)].count)} />
      <div className="wf-review-stops" role="group" aria-label="Review pass presets">
        {presets.map(({ count, Icon }) => {
          const name = `${count} ${count === 1 ? "pass" : "passes"}`;
          return (
            <button type="button" key={count} aria-label={name} title={name}
              aria-pressed={value === count} onClick={() => onChange(count)}>
              <Icon size={20} aria-hidden="true" />
            </button>
          );
        })}
      </div>
    </div>
  );
}
