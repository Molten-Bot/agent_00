async function loadTaskAssetPreview(params, message, { signal, title, token = "" }) {
  const requestID = encodeURIComponent(params.get("request") || "");
  const query = new URLSearchParams({ index: params.get("index") || "" });
  const headers = token ? { Authorization: `Bearer ${token}` } : {};
  const response = await fetch(`/api/tasks/${requestID}/asset-preview?${query}`, { signal, headers, redirect: "error" });
  if (!response.ok) return response.status;

  const blob = await response.blob();
  if (blob.type.startsWith("text/plain")) {
    const text = await blob.text();
    if (signal.aborted) return response.status;
    const preview = document.createElement("pre");
    preview.className = "task-asset-text";
    preview.tabIndex = 0;
    preview.textContent = text;
    message.before(preview);
    message.hidden = true;
    signal.addEventListener("abort", () => preview.remove(), { once: true });
    return response.status;
  }

  const isPDF = blob.type === "application/pdf";
  // Keep SVG in an opaque origin even if opened outside its image element.
  let src;
  if (blob.type === "image/svg+xml") {
    src = await new Promise((resolve, reject) => {
      const reader = new FileReader();
      reader.onload = () => resolve(reader.result);
      reader.onerror = reject;
      reader.readAsDataURL(blob);
    });
  }
  if (signal.aborted) return response.status;
  if (isPDF && navigator.pdfViewerEnabled === false) throw new Error("Preview unavailable.");
  const objectURL = src ? "" : URL.createObjectURL(blob);

  const media = document.createElement(isPDF ? "iframe" : blob.type.startsWith("video/") ? "video" : "img");
  media.className = "task-image-viewer-image";
  media.hidden = !isPDF;
  if (isPDF) {
    message.hidden = true;
    media.title = title;
  } else if (media.tagName === "VIDEO") {
    media.controls = true;
    media.playsInline = true;
    media.preload = "metadata";
    media.setAttribute("aria-label", title);
  } else {
    media.alt = title;
  }
  function removeMedia() {
    media.remove();
    media.removeAttribute("src");
    if (media.tagName === "VIDEO") media.load();
    if (objectURL) URL.revokeObjectURL(objectURL);
  }
  signal.addEventListener("abort", removeMedia, { once: true });
  media.addEventListener(media.tagName === "VIDEO" ? "loadedmetadata" : "load", () => {
    if (!media.isConnected) return;
    message.hidden = true;
    media.hidden = false;
  });
  media.addEventListener("error", () => {
    if (!media.isConnected) return;
    signal.removeEventListener("abort", removeMedia);
    removeMedia();
    message.hidden = false;
    message.textContent = "Preview unavailable.";
  });
  media.src = src || objectURL;
  message.before(media);
  return response.status;
}
