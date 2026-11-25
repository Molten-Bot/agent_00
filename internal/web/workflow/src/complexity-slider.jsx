import React, { useId } from "react";
import { Brain, Rabbit, Sparkles } from "lucide-react";

const levels = [
  { value: "easy", label: "Easy", Icon: Rabbit },
  { value: "standard", label: "Standard", Icon: Brain },
  { value: "advanced", label: "Advanced", Icon: Sparkles },
];

export function ComplexitySlider({ value, defaultComplexity, onChange, label = "Complexity", inheritedLabel = "Use settings default" }) {
  const id = useId();
  const inherited = levels.find((level) => level.value === defaultComplexity) ?? levels[1];
  const stops = [
    { value: undefined, label: `${inheritedLabel} (${inherited.label})`, Icon: inherited.Icon },
    ...levels,
  ];
  const index = Math.max(0, stops.findIndex((stop) => stop.value === value));
  return (
    <div className="wf-field complexity-slider">
      <label htmlFor={id}>{label}</label>
      <input id={id} type="range" min="0" max={stops.length - 1} step="1"
        value={index} title={stops[index].label} aria-valuetext={stops[index].label}
        onChange={(event) => onChange(stops[Number(event.target.value)].value)} />
      <div className="complexity-stops" role="group" aria-label="Complexity presets">
        {stops.map(({ label, Icon, value }, stopIndex) => (
          <button type="button" key={value ?? "default"} title={label} aria-label={label}
            aria-pressed={index === stopIndex} onClick={() => onChange(value)}>
            <Icon aria-hidden="true" />
          </button>
        ))}
      </div>
    </div>
  );
}
