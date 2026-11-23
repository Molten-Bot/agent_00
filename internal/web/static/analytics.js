(() => {
  const measurementID = "G-BY33RFG2WB";
  const pageOrigin = window.location.origin;
  const basePath = window.location.pathname === "/chat" ? "/chat" : "/";
  let pageReferrer = "";
  try {
    const referrer = new URL(document.referrer);
    if (referrer.protocol === "https:" || referrer.protocol === "http:") {
      pageReferrer = referrer.origin + "/";
    }
  } catch {
    // Direct visits have no referrer.
  }
  let lastPageView = "";

  function send(...args) {
    if (window["ga-disable-" + measurementID] === true) return false;
    try {
      window.gtag(...args);
      return true;
    } catch {
      // Keep app actions working when the tag is blocked or fails.
      return false;
    }
  }

  window.dataLayer = window.dataLayer || [];
  window.gtag = window.gtag || function gtag() { window.dataLayer.push(arguments); };
  send("js", new Date());
  send("set", { page_location: pageOrigin + basePath, page_referrer: pageReferrer });
  send("config", measurementID, { send_page_view: false });

  window.trackAnalyticsEvent = function (name, params = {}) {
    const eventName = String(name || "").trim();
    if (!eventName) return false;
    const payload = { send_to: measurementID };
    for (const [key, value] of Object.entries(params || {})) {
      if (typeof value === "string" && value.trim()) {
        payload[key] = value.trim();
      } else if (typeof value === "boolean" || Number.isFinite(value)) {
        payload[key] = value;
      }
    }
    return send("event", eventName, payload);
  };

  // Callers supply a fixed application route, never a value copied from the URL.
  window.trackAnalyticsPageView = function (path, params) {
    const nextLocation = pageOrigin + path;
    if (nextLocation === lastPageView) return;
    const previousLocation = lastPageView || pageReferrer;
    send("set", { page_location: nextLocation, page_referrer: previousLocation });
    if (window.trackAnalyticsEvent("page_view", {
      ...params,
      page_title: document.title,
      page_location: nextLocation,
      page_referrer: previousLocation,
    })) {
      lastPageView = nextLocation;
    }
  };
})();
