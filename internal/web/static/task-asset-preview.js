function fetchTaskAssetPreview(params, { signal, token = "" }) {
  const requestID = encodeURIComponent(params.get("request") || "");
  const headers = { "X-Task-Asset-URL": encodeURIComponent(params.get("asset") || "") };
  if (token) headers.Authorization = `Bearer ${token}`;
  return fetch(`/api/tasks/${requestID}/asset-preview`, { signal, headers, redirect: "error" });
}

async function showTaskAssetPreview(blob, message, { signal, title }) {
  if (signal.aborted) return;
  if (blob.type.startsWith("text/plain")) {
    const text = await blob.text();
    if (signal.aborted) return;
    const preview = document.createElement("pre");
    preview.className = "task-asset-text";
    preview.tabIndex = 0;
    preview.textContent = text;
    message.before(preview);
    message.hidden = true;
    signal.addEventListener("abort", () => preview.remove(), { once: true });
    return;
  }

  const isPDF = blob.type === "application/pdf";
  const isVideo = blob.type.startsWith("video/");
  const isAudio = blob.type.startsWith("audio/");
  if (!isPDF && !isVideo && !isAudio && !blob.type.startsWith("image/")) {
    throw new Error("Preview unavailable.");
  }
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
  if (signal.aborted) return;
  if (isPDF && navigator.pdfViewerEnabled === false) throw new Error("Preview unavailable.");
  const objectURL = src ? "" : URL.createObjectURL(blob);

  const media = document.createElement(isPDF ? "iframe" : isVideo ? "video" : isAudio ? "audio" : "img");
  const hasControls = isVideo || isAudio;
  media.className = "task-image-viewer-image";
  media.hidden = !isPDF;
  if (isPDF) {
    message.hidden = true;
    media.title = title;
  } else if (hasControls) {
    media.controls = true;
    if (isVideo) media.playsInline = true;
    media.preload = "metadata";
    media.setAttribute("aria-label", title);
  } else {
    media.alt = title;
  }
  function removeMedia() {
    media.remove();
    media.removeAttribute("src");
    if (hasControls) media.load();
    if (objectURL) URL.revokeObjectURL(objectURL);
  }
  signal.addEventListener("abort", removeMedia, { once: true });
  media.addEventListener(hasControls ? "loadedmetadata" : "load", () => {
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
}
