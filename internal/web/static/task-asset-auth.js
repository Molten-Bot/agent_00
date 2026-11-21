(() => {
  const params = new URLSearchParams(location.hash.slice(1));
  const form = document.getElementById("task-asset-auth");
  const input = document.getElementById("task-asset-token");
  const message = document.getElementById("task-asset-message");
  const title = document.title;
  const channelName = params.get("channel");
  let channel;
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
      const response = await fetchTaskAssetPreview(params, { signal, token });
      if (signal.aborted) return;
      if (response.ok) {
        const blob = await response.blob();
        await showTaskAssetPreview(blob, message, { signal, title });
        if (signal.aborted || !channelName) return;
        channel = new BroadcastChannel(channelName);
        channel.onmessage = ({ data }) => {
          if (data === "shown") {
            channel.close();
            window.close();
          }
        };
        channel.postMessage(blob);
        return;
      }
      if (response.status === 403) {
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
  window.addEventListener("pagehide", () => {
    input.value = "";
    request?.abort();
    channel?.close();
  });
  if (window.isSecureContext && params.get("request") && params.get("asset")) preview();
})();
