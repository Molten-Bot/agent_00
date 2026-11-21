(() => {
  const params = new URLSearchParams(location.search);
  const form = document.getElementById("task-asset-auth");
  const input = document.getElementById("task-asset-token");
  const message = document.getElementById("task-asset-message");
  const title = document.title;
  let request;

  async function preview(token = "") {
    request?.abort();
    request = new AbortController();
    const { signal } = request;
    form.hidden = true;
    input.value = "";
    message.hidden = false;
    message.textContent = "Loading preview…";
    try {
      const status = await loadTaskAssetPreview(params, message, { signal, title, token });
      if (signal.aborted || status === 200) return;
      if (status === 403) {
        message.textContent = token ? "GitHub denied access to this attachment." : "This attachment requires GitHub access.";
        form.hidden = false;
        input.focus();
        return;
      }
    } catch {
      if (signal.aborted) return;
    }
    message.textContent = "Preview unavailable.";
  }

  form.addEventListener("submit", event => {
    event.preventDefault();
    const token = input.value.trim();
    if (window.isSecureContext && !form.hidden && token) preview(token);
  });
  window.addEventListener("pagehide", () => { input.value = ""; });
  if (window.isSecureContext && params.get("request") && /^\d+$/.test(params.get("index") || "")) preview();
})();
