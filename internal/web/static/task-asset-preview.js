function taskAssetPreviewPath(params) {
  const requestID = encodeURIComponent(params.get("request") || "");
  return `/api/tasks/${requestID}/asset-preview`;
}

function fetchTaskAssetPreview(params, { signal, token = "", nativePlayback = false }) {
  const headers = { "X-Task-Asset-URL": encodeURIComponent(params.get("asset") || "") };
  if (token) headers.Authorization = `Bearer ${token}`;
  if (nativePlayback) headers["X-Task-Asset-Playback"] = "native";
  return fetch(taskAssetPreviewPath(params), { signal, headers, redirect: "error" });
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
  showTaskAssetMedia(blob.type, src || blob, message, { signal, title });
}

function showTaskAssetMedia(mediaType, source, message, { signal, title }) {
  const isPDF = mediaType === "application/pdf";
  const isVideo = mediaType.startsWith("video/");
  const isAudio = mediaType.startsWith("audio/");
  if ((!isPDF && !isVideo && !isAudio && !mediaType.startsWith("image/")) ||
      (isPDF && navigator.pdfViewerEnabled === false)) {
    throw new Error("Preview unavailable.");
  }
  const objectURL = source instanceof Blob ? URL.createObjectURL(source) : "";
  const media = document.createElement(isPDF ? "iframe" : isVideo ? "video" : isAudio ? "audio" : "img");
  const params = { media_type: isPDF ? "pdf" : isVideo ? "video" : isAudio ? "audio" : "image" };
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
    media.addEventListener("playing", () => window.trackAnalyticsEvent?.("task_asset_media_started", params), { once: true });
    media.addEventListener("ended", () => window.trackAnalyticsEvent?.("task_asset_media_completed", params), { once: true });
  } else {
    media.alt = title;
    media.referrerPolicy = "no-referrer";
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
    window.trackAnalyticsEvent?.("task_asset_preview_failed", params);
  });
  media.src = objectURL || source;
  message.before(media);
}

function taskAssetVideoPreview(response) {
  const mediaType = response.headers.get("Content-Type") || "";
  const videoID = response.headers.get("X-Task-Asset-Video");
  if (!mediaType.startsWith("video/") || !/^[a-f0-9]{64}$/.test(videoID)) return null;
  return { mediaType, videoID, playback: response.headers.get("X-Task-Asset-Playback") || "" };
}

function showTaskAssetAuthorizedPreview(data, params, message, options) {
  if (data instanceof Blob) return showTaskAssetPreview(data, message, options);
  if (!data || !/^video\/[a-z0-9.+-]+$/i.test(data.mediaType) ||
      !/^[a-f0-9]{64}$/.test(data.videoID) || !/^[a-zA-Z0-9_-]*$/.test(data.playback)) {
    throw new Error("Invalid video preview");
  }
  const query = new URLSearchParams({ video: data.videoID });
  if (data.playback) query.set("playback", data.playback);
  showTaskAssetMedia(data.mediaType, `${taskAssetPreviewPath(params)}?${query}`, message, options);
}
