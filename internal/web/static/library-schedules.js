(() => {
  const byID = (id) => document.getElementById(`library-schedule-${id}`);
  const modal = byID("modal");
  const editor = byID("editor");
  const list = byID("list");
  const slider = byID("hours");
  const draftInput = byID("draft");
  const draftToggle = draftInput.closest(".prompt-draft-toggle");
  const endpoint = "/api/library/schedules";
  // Snap points: single hours, then widening steps (4h/6h/12h) so the slider
  // offers fewer, rounder choices the further out the repeat interval goes.
  const ALL_HOUR_STEPS = [1, 2, 3, 4, 5, 6, 7, 8, 9, 12, 16, 20, 24, 30, 36, 42, 48, 60, 72, 84, 96, 108, 120, 132, 144, 156, 168];
  let hourSteps = ALL_HOUR_STEPS;
  function hoursForIndex(index) { return hourSteps[Number(index)] ?? hourSteps[0]; }
  function indexForHours(hours) {
    let closest = 0;
    let closestDiff = Infinity;
    hourSteps.forEach((value, index) => {
      const diff = Math.abs(value - Number(hours));
      if (diff < closestDiff) { closestDiff = diff; closest = index; }
    });
    return closest;
  }
  function applyHourSteps() {
    slider.min = "0";
    slider.max = String(hourSteps.length - 1);
    byID("range-min").textContent = `${hourSteps[0]} hour${hourSteps[0] === 1 ? "" : "s"}`;
    const max = hourSteps[hourSteps.length - 1];
    byID("range-max").textContent = max >= 168 ? `${max} hours · 1 week` : `${max} hours`;
  }
  applyHourSteps();
  let schedules = [];
  let editing = null;
  let busy = false;
  let opener = null;

  function status(message, error = false) {
    byID("status").textContent = message;
    byID("status").classList.toggle("is-error", error);
  }
  function titleFor(name) {
    const task = (state.libraryTasks || []).find((task) => task.name === name);
    return task ? libraryTaskDisplayName(task) : name;
  }
  function frequency(hours) { return `every ${hours} ${Number(hours) === 1 ? "hour" : "hours"}`; }
  function nextText(entry) {
    const next = new Date(entry.nextRunAt);
    return next <= new Date() ? "Due · waiting for the next available run" : `Next run ${next.toLocaleString()}`;
  }
  function refreshButtons() {
    document.getElementById("library-schedules-count").textContent = schedules.length ? String(schedules.length) : "";
    const repo = repoDedupeKey(libraryRepoInput.value || defaultRepository());
    for (const button of document.querySelectorAll(".library-schedule-task-button")) {
      if (!button.dataset.taskName) continue;
      const entry = schedules.find((entry) => entry.libraryTaskName === button.dataset.taskName && repoDedupeKey(entry.repo) === repo);
      button.classList.toggle("is-scheduled", Boolean(entry));
      const label = entry ? `Edit schedule · ${frequency(entry.intervalHours)}` : `Schedule ${titleFor(button.dataset.taskName)}`;
      button.title = label;
      button.setAttribute("aria-label", label);
    }
  }
  async function request(options = {}, suffix = "") {
    const response = await fetch(endpoint + suffix, { cache: "no-store", ...options });
    const body = await response.json();
    if (!response.ok || body.ok === false) throw new Error(body.error || "Unable to save schedule.");
    return body;
  }
  async function load() {
    const body = await request();
    schedules = body.schedules || [];
    hourSteps = ALL_HOUR_STEPS.filter((hours) => hours <= (body.maxIntervalHours || 168));
    applyHourSteps();
    refreshButtons();
  }
  function iconButton(icon, label, action) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "library-task-option-link";
    button.title = label;
    button.setAttribute("aria-label", label);
    button.innerHTML = `<i data-lucide="${icon}" aria-hidden="true"></i>`;
    button.addEventListener("click", action);
    return button;
  }
  function showList() {
    editing = null;
    editor.hidden = true;
    list.hidden = false;
    byID("title").textContent = "Schedules";
    list.replaceChildren();
    if (!schedules.length) {
      const empty = document.createElement("p");
      empty.className = "library-schedule-copy";
      empty.textContent = "No schedules yet. Use the clock next to the Run button to add one. Run still starts a one-time task.";
      list.appendChild(empty);
    }
    for (const entry of schedules) {
      const row = document.createElement("div");
      row.className = "library-schedule-row";
      const details = document.createElement("div");
      const name = document.createElement("strong");
      name.textContent = titleFor(entry.libraryTaskName);
      details.appendChild(name);
      for (const value of [displayRepoLabel(entry.repo), frequency(entry.intervalHours), nextText(entry)]) {
        const line = document.createElement("div");
        line.className = "library-schedule-copy";
        line.textContent = value;
        details.appendChild(line);
      }
      if (entry.lastError) {
        const error = document.createElement("div");
        error.className = "library-schedule-copy is-error";
        error.textContent = `Last attempt: ${entry.lastError}`;
        details.appendChild(error);
      }
      row.append(details,
        iconButton("pencil", `Edit schedule for ${titleFor(entry.libraryTaskName)}`, () => showEditor(entry)),
        iconButton("trash-2", `Delete schedule for ${titleFor(entry.libraryTaskName)}`, () => remove(entry)));
      list.appendChild(row);
    }
    replaceLucideIcons(list);
  }
  function updateInterval(changed = false) {
    const hours = hoursForIndex(slider.value);
    byID("frequency").textContent = frequency(hours);
    slider.setAttribute("aria-valuetext", frequency(hours));
    byID("next").textContent = changed || !editing?.nextRunAt
      ? `First run ${hours} ${hours === 1 ? "hour" : "hours"} after saving. Saving resets the next run time.`
      : nextText(editing);
  }
  function showEditor(entry) {
    status("");
    editing = { ...entry };
    list.hidden = true;
    editor.hidden = false;
    byID("title").textContent = entry.key ? "Edit schedule" : "Schedule task";
    byID("task").textContent = titleFor(entry.libraryTaskName);
    byID("repo").textContent = displayRepoLabel(entry.repo);
    byID("options").textContent = `Branch: ${entry.baseBranch || "default"} · Directory: ${entry.targetSubdir || "."}` + (entry.reviewers?.length ? ` · Reviewers: ${entry.reviewers.join(", ")}` : "");
    draftInput.checked = Boolean(entry.draft);
    draftToggle?.classList.toggle("is-active", draftInput.checked);
    byID("delete").hidden = !entry.key;
    slider.value = String(indexForHours(entry.intervalHours || 24));
    updateInterval();
    slider.focus();
  }
  function open() {
    opener = document.activeElement;
    if (!modal.open) modal.showModal();
    status("Loading schedules…");
  }
  async function openFor(entry) {
    open();
    editor.hidden = true;
    list.hidden = true;
    try {
      const payload = libraryPayload(entry.name);
      await load();
      const existing = schedules.find((schedule) => schedule.libraryTaskName === entry.name && repoDedupeKey(schedule.repo) === repoDedupeKey(payload.repos[0]));
      showEditor(existing || { libraryTaskName: entry.name, repo: payload.repos[0], baseBranch: payload.branch || "", targetSubdir: payload.targetsubdir || ".", reviewers: payload.reviewers || [], draft: Boolean(payload.draft), intervalHours: 24 });
    } catch (error) { status(error.message, true); }
  }
  async function mutate(action, success) {
    if (busy) return;
    busy = true;
    for (const control of modal.querySelectorAll("button, input")) control.disabled = true;
    status("Saving…");
    try {
      await action();
      await load();
      showList();
      status(success);
    } catch (error) { status(error.message, true); }
    finally {
      busy = false;
      for (const control of modal.querySelectorAll("button, input")) control.disabled = false;
    }
  }
  function remove(entry) {
    return mutate(() => request({ method: "DELETE" }, `?key=${encodeURIComponent(entry.key)}`), "Schedule deleted. Any run already started will continue.");
  }
  document.getElementById("library-schedules-open").addEventListener("click", async () => {
    open();
    editor.hidden = true;
    list.hidden = true;
    try { await load(); showList(); status(""); } catch (error) { status(error.message, true); }
  });
  byID("close").addEventListener("click", () => modal.close());
  modal.addEventListener("close", () => opener?.focus());
  modal.addEventListener("cancel", (event) => { if (busy) event.preventDefault(); });
  byID("back").addEventListener("click", () => { showList(); status(""); });
  draftInput.addEventListener("change", () => draftToggle?.classList.toggle("is-active", draftInput.checked));
  byID("delete").addEventListener("click", () => { if (editing?.key) void remove(editing); });
  slider.addEventListener("input", () => updateInterval(true));
  editor.addEventListener("submit", (event) => {
    event.preventDefault();
    if (!editing) return;
    const { libraryTaskName, repo, baseBranch, targetSubdir, reviewers } = editing;
    const draft = byID("draft").checked;
    void mutate(() => request({ method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ libraryTaskName, repo, baseBranch, targetSubdir, reviewers, draft, intervalHours: hoursForIndex(slider.value) }) }), "Schedule saved. You can still use Run for a one-time task.");
  });
  document.addEventListener("change", refreshButtons);
  libraryRepoInput.addEventListener("input", refreshButtons);
  window.LibrarySchedulesUI = { openFor, refreshButtons };
  void load().catch(() => {}); // Report load failures when the schedules dialog is opened.
})();
