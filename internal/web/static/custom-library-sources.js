(() => {
  const nameInput = document.getElementById("custom-library-name");
  const repoInput = document.getElementById("custom-library-repo");
  const addButton = document.getElementById("custom-library-add");
  const status = document.getElementById("custom-library-status");
  const list = document.getElementById("custom-library-list");
  if (!nameInput || !repoInput || !addButton || !status || !list) return;

  const endpoint = "/api/library/sources";
  let busy = false;

  function setBusy(value) {
    busy = value;
    addButton.disabled = value;
    nameInput.disabled = value;
    repoInput.disabled = value;
    for (const button of list.querySelectorAll("button")) button.disabled = value;
  }

  function setStatus(message, isError) {
    status.textContent = message || "";
    status.style.color = isError ? "var(--surface-danger)" : "";
  }

  function formatStatusLine(entry) {
    if (entry.lastError) return `Sync failed: ${entry.lastError}`;
    if (!entry.syncedAt) return "Not yet synced.";
    const count = Number(entry.taskCount || 0);
    const tasks = count === 1 ? "1 task" : `${count} tasks`;
    return `${tasks} · last synced ${new Date(entry.syncedAt).toLocaleString()}`;
  }

  function render(entries) {
    list.replaceChildren();
    if (!entries || !entries.length) {
      const empty = document.createElement("p");
      empty.className = "library-schedule-copy";
      empty.textContent = "No company library repos configured yet.";
      list.appendChild(empty);
      return;
    }
    for (const entry of entries) {
      const row = document.createElement("div");
      row.className = "library-schedule-row";

      const details = document.createElement("div");
      const title = document.createElement("strong");
      title.textContent = entry.name;
      details.appendChild(title);
      const repoLine = document.createElement("div");
      repoLine.className = "library-schedule-copy";
      repoLine.textContent = entry.repo;
      details.appendChild(repoLine);
      const statusLine = document.createElement("div");
      statusLine.className = "library-schedule-copy" + (entry.lastError ? " is-error" : "");
      statusLine.textContent = formatStatusLine(entry);
      details.appendChild(statusLine);
      row.appendChild(details);

      const removeButton = document.createElement("button");
      removeButton.type = "button";
      removeButton.className = "library-task-option-link";
      removeButton.title = `Remove ${entry.name}`;
      removeButton.setAttribute("aria-label", `Remove ${entry.name}`);
      removeButton.innerHTML = '<i data-lucide="trash-2" aria-hidden="true"></i>';
      removeButton.addEventListener("click", () => void removeSource(entry.name));
      row.appendChild(removeButton);

      list.appendChild(row);
    }
    if (window.lucide?.createIcons) window.lucide.createIcons();
  }

  async function request(method, suffix, body) {
    const response = await fetch(endpoint + suffix, {
      method,
      cache: "no-store",
      ...(body === undefined ? {} : { headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) }),
    });
    const payload = await response.json();
    if (!response.ok || payload.ok === false) throw new Error(payload.error || "Unable to update the company library.");
    return payload;
  }

  async function refreshEntries() {
    const payload = await request("GET", "");
    render(payload.sources || []);
  }

  async function load() {
    if (busy) return;
    setBusy(true);
    try {
      await refreshEntries();
    } catch (error) {
      setStatus(error.message || "Unable to load company library repos.", true);
    } finally {
      setBusy(false);
    }
  }

  async function addSource() {
    if (busy) return;
    const name = nameInput.value.trim();
    const repo = repoInput.value.trim();
    if (!name || !repo) {
      setStatus("Name and repository are both required.", true);
      return;
    }
    setBusy(true);
    setStatus("Cloning and validating the repository…");
    try {
      const payload = await request("POST", "", { name, repo });
      nameInput.value = "";
      repoInput.value = "";
      const count = Number(payload.source?.taskCount || 0);
      setStatus(`Added ${payload.source.name} (${count} task${count === 1 ? "" : "s"}).`);
      await refreshEntries();
    } catch (error) {
      setStatus(error.message || "Unable to add this library repo.", true);
    } finally {
      setBusy(false);
    }
  }

  async function removeSource(name) {
    if (busy) return;
    setBusy(true);
    setStatus(`Removing ${name}…`);
    try {
      await request("DELETE", `?name=${encodeURIComponent(name)}`);
      setStatus(`Removed ${name}.`);
      await refreshEntries();
    } catch (error) {
      setStatus(error.message || "Unable to remove this library repo.", true);
    } finally {
      setBusy(false);
    }
  }

  addButton.addEventListener("click", () => void addSource());
  repoInput.addEventListener("keydown", (event) => {
    if (event.key === "Enter") {
      event.preventDefault();
      void addSource();
    }
  });

  window.addEventListener("custom-library-sources-open", load);
  if (!document.getElementById("settings-gate").classList.contains("hidden")) void load();
})();
