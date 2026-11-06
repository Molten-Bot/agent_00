(function initMarkdownView(global) {
  "use strict";

  const MARKED_SCRIPT_URL = "/static/marked.umd.js";

  // Issue and comment bodies are author-controlled text from GitHub, so the HTML
  // marked produces is filtered through an allow list before it reaches the DOM.
  // Anything outside the list is unwrapped to its text, and the tags that can
  // execute or load code are dropped with their contents.
  const ALLOWED_ELEMENTS = new Map([
    ["A", ["href", "title"]],
    ["BLOCKQUOTE", []],
    ["BR", []],
    ["CODE", ["class"]],
    ["DEL", []],
    ["DD", []],
    ["DETAILS", ["open"]],
    ["DL", []],
    ["DT", []],
    ["EM", []],
    ["H1", []],
    ["H2", []],
    ["H3", []],
    ["H4", []],
    ["H5", []],
    ["H6", []],
    ["HR", []],
    ["IMG", ["alt", "src", "title"]],
    ["INPUT", ["checked", "disabled", "type"]],
    ["KBD", []],
    ["LI", []],
    ["OL", ["start"]],
    ["P", []],
    ["PRE", []],
    ["STRONG", []],
    ["SUB", []],
    ["SUMMARY", []],
    ["SUP", []],
    ["TABLE", []],
    ["TBODY", []],
    ["TD", ["align"]],
    ["TH", ["align"]],
    ["THEAD", []],
    ["TR", []],
    ["UL", []],
  ]);

  const DROPPED_ELEMENTS = new Set([
    "AUDIO", "BUTTON", "CANVAS", "EMBED", "FORM", "IFRAME", "LINK", "MATH", "META",
    "NOSCRIPT", "OBJECT", "SCRIPT", "SELECT", "STYLE", "SVG", "TEMPLATE", "TEXTAREA", "VIDEO",
  ]);

  const SAFE_LINK_SCHEMES = new Set(["http:", "https:", "mailto:"]);
  const SAFE_IMAGE_SCHEMES = new Set(["http:", "https:"]);
  // A caller's base only means something as an absolute web page address.
  const SAFE_BASE_SCHEMES = new Set(["http:", "https:"]);

  let markedScriptPromise = null;

  function markedGlobal() {
    return global.marked && typeof global.marked.parse === "function" ? global.marked : null;
  }

  function loadMarked() {
    const loaded = markedGlobal();
    if (loaded) {
      return Promise.resolve(loaded);
    }
    if (markedScriptPromise) {
      return markedScriptPromise;
    }

    markedScriptPromise = new Promise((resolve, reject) => {
      const finish = () => {
        const marked = markedGlobal();
        if (marked) resolve(marked);
        else reject(new Error("marked script loaded without defining marked"));
      };
      const settle = (node) => {
        node.addEventListener("load", () => {
          node.setAttribute("data-marked-script-state", "loaded");
          finish();
        }, { once: true });
        node.addEventListener("error", () => {
          node.remove();
          reject(new Error("marked script failed to load"));
        }, { once: true });
      };
      // A load event fires once, so waiting on a script element that already
      // finished would leave this promise pending forever. A node that recorded
      // its load settles from that state instead, and a leftover node of unknown
      // state is replaced: a load still in flight is reached through this
      // promise, so it is never the node found here.
      const existing = global.document.querySelector('script[data-marked-script="true"]');
      if (existing?.getAttribute("data-marked-script-state") === "loaded") {
        finish();
        return;
      }
      existing?.remove();
      const script = global.document.createElement("script");
      script.src = MARKED_SCRIPT_URL;
      script.async = true;
      script.setAttribute("data-marked-script", "true");
      settle(script);
      global.document.head.appendChild(script);
    }).catch((error) => {
      // A failed load must not be cached, or every later body would stay plain
      // text for the lifetime of the page.
      markedScriptPromise = null;
      throw error;
    });

    return markedScriptPromise;
  }

  // safeURL resolves the scheme the browser would actually use. Whitespace and
  // control characters are stripped first because browsers ignore them, so
  // "java\nscript:" would otherwise slip past a plain scheme test. Schemeless
  // values are relative to wherever the markdown came from, so they resolve
  // against baseURL; without one they would silently point at the harness
  // origin, which is never what the author wrote.
  function safeURL(value, schemes, baseURL) {
    const raw = String(value || "").trim();
    const collapsed = raw.replace(/[\u0000-\u0020\u007f]/g, "");
    if (!collapsed) return "";
    const scheme = collapsed.match(/^([a-z][a-z0-9+.-]*):/i);
    if (scheme) return schemes.has(`${scheme[1].toLowerCase()}:`) ? raw : "";
    if (!baseURL) return "";
    try {
      const resolved = new URL(raw, baseURL);
      return schemes.has(resolved.protocol) ? resolved.href : "";
    } catch (_error) {
      return "";
    }
  }

  function copyAttributes(source, target, tag, baseURL) {
    for (const name of ALLOWED_ELEMENTS.get(tag) || []) {
      if (!source.hasAttribute(name)) continue;
      const value = source.getAttribute(name);
      if (tag === "A" && name === "href") {
        const href = safeURL(value, SAFE_LINK_SCHEMES, baseURL);
        if (href) target.setAttribute("href", href);
        continue;
      }
      if (tag === "IMG" && name === "src") {
        // No base for images: GitHub serves a relative image from a raw path of
        // the branch it was committed on, which this cannot reconstruct, so a
        // resolved guess would only render as a broken image.
        const src = safeURL(value, SAFE_IMAGE_SCHEMES);
        if (src) target.setAttribute("src", src);
        continue;
      }
      if (name === "class") {
        // marked only puts language hints on code elements; nothing else may
        // borrow application styling.
        if (/^language-[\w.+-]+$/.test(String(value || ""))) target.setAttribute("class", value);
        continue;
      }
      target.setAttribute(name, value);
    }
    if (tag === "A") {
      target.target = "_blank";
      target.rel = "noopener noreferrer";
    }
    if (tag === "IMG") {
      target.loading = "lazy";
      target.referrerPolicy = "no-referrer";
    }
    if (tag === "INPUT") {
      // Task list checkboxes are the only inputs markdown produces, and they
      // stay read-only here.
      if (String(target.getAttribute("type") || "").toLowerCase() !== "checkbox") return false;
      target.setAttribute("disabled", "");
    }
    return true;
  }

  function appendSanitized(target, node, doc, baseURL) {
    if (node.nodeType === 3) {
      target.appendChild(doc.createTextNode(node.nodeValue));
      return;
    }
    if (node.nodeType !== 1) return;
    const tag = node.tagName.toUpperCase();
    if (DROPPED_ELEMENTS.has(tag)) return;
    if (!ALLOWED_ELEMENTS.has(tag)) {
      for (const child of Array.from(node.childNodes)) appendSanitized(target, child, doc, baseURL);
      return;
    }
    const clean = doc.createElement(tag.toLowerCase());
    if (copyAttributes(node, clean, tag, baseURL) === false) return;
    if (tag === "IMG" && !clean.hasAttribute("src")) {
      // The source was rejected, but the alt text is author prose worth keeping.
      const alt = String(node.getAttribute("alt") || "").trim();
      if (alt) target.appendChild(doc.createTextNode(alt));
      return;
    }
    for (const child of Array.from(node.childNodes)) appendSanitized(clean, child, doc, baseURL);
    target.appendChild(clean);
  }

  // options.baseURL is the page the markdown was written on, for example the
  // repository an issue belongs to, and relative links resolve against it.
  function sanitize(html, ownerDocument, options) {
    const doc = ownerDocument || global.document;
    const baseURL = safeURL(options?.baseURL, SAFE_BASE_SCHEMES);
    const parsed = new DOMParser().parseFromString(String(html || ""), "text/html");
    const fragment = doc.createDocumentFragment();
    for (const child of Array.from(parsed.body.childNodes)) appendSanitized(fragment, child, doc, baseURL);
    return fragment;
  }

  function toHTML(text) {
    return global.marked.parse(String(text || ""), { async: false, gfm: true, breaks: true });
  }

  // render fills element with text as plain text first so the body is readable
  // even if marked never loads, then swaps in the rendered markdown.
  function render(element, text, options) {
    if (!element) return Promise.resolve(false);
    const source = String(text || "");
    element.textContent = source;
    element.classList.add("markdown-body", "markdown-body-pending");
    return loadMarked()
      .then(() => {
        const doc = element.ownerDocument || global.document;
        element.replaceChildren(sanitize(toHTML(source), doc, options));
        element.classList.remove("markdown-body-pending");
        return true;
      })
      .catch((error) => {
        global.console?.warn?.("markdown render failed, showing plain text", error);
        return false;
      });
  }

  global.MarkdownView = { render, sanitize };
})(window);
