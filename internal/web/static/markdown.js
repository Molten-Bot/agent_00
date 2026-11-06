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

  let markedScriptPromise = null;

  function loadMarked() {
    if (global.marked && typeof global.marked.parse === "function") {
      return Promise.resolve(global.marked);
    }
    if (markedScriptPromise) {
      return markedScriptPromise;
    }

    markedScriptPromise = new Promise((resolve, reject) => {
      const settle = (node) => {
        node.addEventListener("load", () => resolve(global.marked), { once: true });
        node.addEventListener("error", () => reject(new Error("marked script failed to load")), { once: true });
      };
      const existing = global.document.querySelector('script[data-marked-script="true"]');
      if (existing) {
        settle(existing);
        return;
      }
      const script = global.document.createElement("script");
      script.src = MARKED_SCRIPT_URL;
      script.async = true;
      script.setAttribute("data-marked-script", "true");
      settle(script);
      global.document.head.appendChild(script);
    });

    return markedScriptPromise;
  }

  // safeURL resolves the scheme the browser would actually use. Whitespace and
  // control characters are stripped first because browsers ignore them, so
  // "java\nscript:" would otherwise slip past a plain scheme test. Schemeless
  // values are relative, which is safe for a link but never a real image, so
  // requireScheme refuses them.
  function safeURL(value, schemes, requireScheme) {
    const raw = String(value || "").trim();
    const collapsed = raw.replace(/[\u0000-\u0020\u007f]/g, "");
    if (!collapsed) return "";
    const scheme = collapsed.match(/^([a-z][a-z0-9+.-]*):/i);
    if (!scheme) return requireScheme ? "" : raw;
    return schemes.has(`${scheme[1].toLowerCase()}:`) ? raw : "";
  }

  function copyAttributes(source, target, tag) {
    for (const name of ALLOWED_ELEMENTS.get(tag) || []) {
      if (!source.hasAttribute(name)) continue;
      const value = source.getAttribute(name);
      if (tag === "A" && name === "href") {
        const href = safeURL(value, SAFE_LINK_SCHEMES);
        if (href) target.setAttribute("href", href);
        continue;
      }
      if (tag === "IMG" && name === "src") {
        const src = safeURL(value, SAFE_IMAGE_SCHEMES, true);
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

  function appendSanitized(target, node, doc) {
    if (node.nodeType === 3) {
      target.appendChild(doc.createTextNode(node.nodeValue));
      return;
    }
    if (node.nodeType !== 1) return;
    const tag = node.tagName.toUpperCase();
    if (DROPPED_ELEMENTS.has(tag)) return;
    if (!ALLOWED_ELEMENTS.has(tag)) {
      for (const child of Array.from(node.childNodes)) appendSanitized(target, child, doc);
      return;
    }
    const clean = doc.createElement(tag.toLowerCase());
    if (copyAttributes(node, clean, tag) === false) return;
    if (tag === "IMG" && !clean.hasAttribute("src")) return;
    for (const child of Array.from(node.childNodes)) appendSanitized(clean, child, doc);
    target.appendChild(clean);
  }

  function sanitize(html, ownerDocument) {
    const doc = ownerDocument || global.document;
    const parsed = new DOMParser().parseFromString(String(html || ""), "text/html");
    const fragment = doc.createDocumentFragment();
    for (const child of Array.from(parsed.body.childNodes)) appendSanitized(fragment, child, doc);
    return fragment;
  }

  function toHTML(text) {
    return global.marked.parse(String(text || ""), { async: false, gfm: true, breaks: true });
  }

  // render fills element with text as plain text first so the body is readable
  // even if marked never loads, then swaps in the rendered markdown.
  function render(element, text) {
    if (!element) return Promise.resolve(false);
    const source = String(text || "");
    element.textContent = source;
    element.classList.add("markdown-body-pending");
    return loadMarked()
      .then(() => {
        const doc = element.ownerDocument || global.document;
        element.replaceChildren(sanitize(toHTML(source), doc));
        element.classList.remove("markdown-body-pending");
        return true;
      })
      .catch(() => false);
  }

  global.MarkdownView = { render, sanitize };
})(window);
