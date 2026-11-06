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
  // Fetching an image tells its host the operator's IP address and user agent
  // before anyone chose to trust the author. GitHub already serves this page's
  // data, so its own hosts carry no new disclosure; every other host is left as
  // a link the reader opens deliberately.
  const AUTO_LOADED_IMAGE_HOSTS = ["github.com", "githubusercontent.com"];

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
      const finish = (node) => {
        const marked = markedGlobal();
        if (marked) {
          // Only a script that actually defined marked is worth reusing, so the
          // state is recorded here rather than on the load event.
          node?.setAttribute("data-marked-script-state", "loaded");
          resolve(marked);
          return;
        }
        // A script that ran without defining marked is not a usable load, and
        // leaving it in the document would make every later render fail against
        // it. Dropping it lets the next render fetch the bundle again.
        node?.remove();
        reject(new Error("marked script loaded without defining marked"));
      };
      const settle = (node) => {
        node.addEventListener("load", () => finish(node), { once: true });
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
        finish(existing);
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

  function autoLoadableImage(url) {
    try {
      const host = new URL(url).hostname.toLowerCase();
      return AUTO_LOADED_IMAGE_HOSTS.some((trusted) => host === trusted || host.endsWith(`.${trusted}`));
    } catch (_error) {
      return false;
    }
  }

  // GitHub turns a heading into an anchor by slugging its text, which is how a
  // fragment an author wrote resolves to a heading here as well. Headings carry
  // no id of their own: an id taken from author text would collide with the
  // application's own elements.
  function headingSlug(text) {
    return String(text || "").trim().toLowerCase().replace(/[^\w\- ]+/g, "").replace(/\s+/g, "-");
  }

  // A fragment addresses the document it was written in, so it stays one. The
  // click is handled here because letting it reach the location bar would hand
  // issue text control of the application's own hash routing.
  function scrollToFragment(event) {
    event.preventDefault();
    const anchor = event.currentTarget;
    const raw = String(anchor.getAttribute("href") || "").slice(1);
    if (!raw) return;
    let wanted = raw;
    try {
      wanted = decodeURIComponent(raw);
    } catch (_error) {
      // A malformed escape is matched as written.
    }
    wanted = headingSlug(wanted);
    const root = anchor.closest(".markdown-body") || anchor.parentNode;
    const headings = root?.querySelectorAll?.("h1, h2, h3, h4, h5, h6") || [];
    Array.from(headings)
      .find((heading) => headingSlug(heading.textContent) === wanted)
      ?.scrollIntoView({ block: "start" });
  }

  function copyAttributes(source, target, tag, baseURL) {
    let fragmentLink = false;
    for (const name of ALLOWED_ELEMENTS.get(tag) || []) {
      if (!source.hasAttribute(name)) continue;
      const value = source.getAttribute(name);
      if (tag === "A" && name === "href") {
        const raw = String(value || "").trim();
        if (raw.startsWith("#")) {
          fragmentLink = true;
          target.setAttribute("href", raw);
          continue;
        }
        const href = safeURL(value, SAFE_LINK_SCHEMES, baseURL);
        if (href) target.setAttribute("href", href);
        continue;
      }
      if (tag === "IMG" && name === "src") {
        // No base for images: GitHub serves a relative image from a raw path of
        // the branch it was committed on, which this cannot reconstruct, so a
        // resolved guess would only render as a broken image.
        const src = safeURL(value, SAFE_IMAGE_SCHEMES);
        if (src && autoLoadableImage(src)) target.setAttribute("src", src);
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
    if (tag === "A" && fragmentLink) {
      target.addEventListener("click", scrollToFragment);
    } else if (tag === "A") {
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
      const alt = String(node.getAttribute("alt") || "").trim();
      const held = safeURL(node.getAttribute("src"), SAFE_IMAGE_SCHEMES);
      if (held) {
        // The address is fetchable but off GitHub, so it becomes a link rather
        // than a request the operator never asked to make.
        const link = doc.createElement("a");
        link.setAttribute("href", held);
        link.target = "_blank";
        link.rel = "noopener noreferrer";
        link.className = "markdown-remote-image";
        link.textContent = alt || held;
        target.appendChild(link);
        return;
      }
      // The source was rejected, but the alt text is author prose worth keeping.
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
