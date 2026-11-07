(function initMarkdownView(global) {
  "use strict";

  const MARKED_SCRIPT_URL = "/static/marked.umd.js";

  // Issue and comment bodies are author-controlled text from GitHub, so the HTML
  // marked produces is filtered through an allow list before it reaches the DOM.
  // Anything outside the list is unwrapped to its text, and only the tags whose
  // contents are code or an embedded resource are dropped with those contents.
  const ALLOWED_ELEMENTS = new Map([
    ["A", ["href", "title"]],
    ["B", []],
    ["BLOCKQUOTE", []],
    ["BR", []],
    ["CAPTION", []],
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
    ["I", []],
    ["IMG", ["alt", "src", "title"]],
    ["INPUT", ["checked", "disabled", "type"]],
    ["KBD", []],
    ["LI", []],
    ["OL", ["start"]],
    ["P", []],
    ["PRE", []],
    ["S", []],
    ["SPAN", []],
    ["STRONG", []],
    ["SUB", []],
    ["SUMMARY", []],
    ["SUP", []],
    ["TABLE", []],
    ["TBODY", []],
    ["TD", ["align"]],
    ["TH", ["align"]],
    ["THEAD", []],
    ["TFOOT", []],
    ["TR", []],
    ["U", []],
    ["UL", []],
  ]);

  // Dropping one of these with its contents is the only way to keep stylesheet
  // or script source out of the page as text. Interactive containers such as
  // form, button, select, and textarea are deliberately absent: their markup
  // means nothing here, but the words inside them are the author's, and this
  // pane offers no raw view to read them in instead, so they are unwrapped like
  // any other unknown tag.
  const DROPPED_ELEMENTS = new Set([
    "AUDIO", "CANVAS", "EMBED", "IFRAME", "LINK", "MATH", "META",
    "OBJECT", "SCRIPT", "STYLE", "SVG", "TEMPLATE", "VIDEO",
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
  // Where a link's fragment is held once its href points at the page the
  // markdown came from.
  const FRAGMENT_ATTRIBUTE = "data-markdown-fragment";

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
      // A load event fires once, so waiting on a script element that already
      // finished would leave this promise pending forever. Any leftover element
      // is replaced instead: marked is undefined here, so a usable load never
      // produced that element, and a load still in flight is reached through
      // this promise rather than through the document.
      global.document.querySelector('script[data-marked-script="true"]')?.remove();
      const script = global.document.createElement("script");
      script.src = MARKED_SCRIPT_URL;
      script.async = true;
      script.setAttribute("data-marked-script", "true");
      script.addEventListener("load", () => {
        const marked = markedGlobal();
        if (marked) {
          resolve(marked);
          return;
        }
        // A script that ran without defining marked is not a usable load, and
        // leaving it in the document would make every later render fail against
        // it. Dropping it lets the next render fetch the bundle again.
        script.remove();
        reject(new Error("marked script loaded without defining marked"));
      }, { once: true });
      script.addEventListener("error", () => {
        script.remove();
        reject(new Error("marked script failed to load"));
      }, { once: true });
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
    return String(text || "").trim().toLowerCase()
      .replace(/[^\p{L}\p{M}\p{N}_\-\s]+/gu, "")
      .replace(/\s+/g, "-");
  }

  // A fragment addresses the document it was written in, so a plain click stays
  // in this body. The click is handled here because letting it reach the
  // location bar would hand issue text control of the application's own hash
  // routing.
  function scrollToFragment(event) {
    // A modifier or middle click asks for a new tab or window, which this body
    // cannot be. The href is the author's own page, so the browser is left to
    // follow it there instead of scrolling here.
    if ((event.button ?? 0) !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    const anchor = event.currentTarget;
    const raw = String(anchor.getAttribute(FRAGMENT_ATTRIBUTE) || "");
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
    const usedSlugs = new Set();
    const heading = Array.from(headings).find((candidate) => {
      const base = headingSlug(candidate.textContent);
      let slug = base;
      let suffix = 0;
      while (usedSlugs.has(slug)) slug = `${base}-${++suffix}`;
      usedSlugs.add(slug);
      return slug === wanted;
    });
    // A fragment can name something other than a heading on the source page.
    // Only intercept links this body can satisfy; otherwise its normal href is
    // still useful and must remain clickable.
    if (!heading) return;
    event.preventDefault();
    heading.scrollIntoView({ block: "start" });
  }

  // A fragment link that never got an href is not a link to the browser: it
  // answers no key press, so the keys that activate a link are routed to the
  // same scroll a click takes.
  function activateFragment(event) {
    if (event.key !== "Enter" && event.key !== " " && event.key !== "Spacebar") return;
    if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    scrollToFragment(event);
  }

  function proxiedImageURL(value, imageProxyURL) {
    const raw = String(value || "").trim();
    if (!raw || /^[a-z][a-z0-9+.-]*:/i.test(raw) || raw.startsWith("//")) return "";
    try {
      const proxy = new URL(imageProxyURL);
      if (!global.location?.origin || proxy.origin !== global.location.origin) return "";
      const repositoryPath = new URL(raw, "https://repository.invalid/").pathname.replace(/^\/+/, "");
      if (!repositoryPath) return "";
      proxy.searchParams.set("path", decodeURIComponent(repositoryPath));
      return proxy.href;
    } catch (_error) {
      return "";
    }
  }

  function copyAttributes(source, target, tag, baseURL, imageBaseURL, imageProxyURL) {
    let fragment = null;
    for (const name of ALLOWED_ELEMENTS.get(tag) || []) {
      if (!source.hasAttribute(name)) continue;
      const value = source.getAttribute(name);
      if (tag === "A" && name === "href") {
        const raw = String(value || "").trim();
        if (raw.startsWith("#")) {
          // The fragment is kept apart from the href so the scroll target stays
          // the author's own anchor name. The href resolves against the page the
          // markdown came from, which is where a new-tab click belongs; leaving
          // the bare fragment there would point it at the application's own hash
          // route. Without a base there is no such page, so there is no href.
          fragment = raw.slice(1);
          const resolved = safeURL(raw, SAFE_LINK_SCHEMES, baseURL);
          if (resolved) target.setAttribute("href", resolved);
          continue;
        }
        const href = safeURL(value, SAFE_LINK_SCHEMES, baseURL);
        if (href) target.setAttribute("href", href);
        continue;
      }
      if (tag === "IMG" && name === "src") {
        // Relative repository images use authenticated content when available.
        // Resolving them against the page URL would return HTML, not image bytes.
        const proxied = proxiedImageURL(value, imageProxyURL);
        const src = proxied || safeURL(value, SAFE_IMAGE_SCHEMES, imageBaseURL);
        if (src && (proxied || autoLoadableImage(src))) target.setAttribute("src", src);
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
    if (tag === "A" && fragment !== null) {
      target.setAttribute(FRAGMENT_ATTRIBUTE, fragment);
      target.rel = "noopener noreferrer";
      target.addEventListener("click", scrollToFragment);
      if (!target.hasAttribute("href")) {
        // Focus order and the link role both come from the href, so an anchor
        // left without one is given them here rather than being reachable by
        // pointer alone.
        target.setAttribute("role", "link");
        target.setAttribute("tabindex", "0");
        target.addEventListener("keydown", activateFragment);
      }
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

  // An image left unfetched still has author text behind it, and its address is
  // worth offering as a link the reader opens deliberately.
  function appendUnloadedImage(target, node, doc) {
    const alt = String(node.getAttribute("alt") || "").trim();
    const held = safeURL(node.getAttribute("src"), SAFE_IMAGE_SCHEMES);
    // Inside a link the author already chose where a click goes. A nested
    // anchor is invalid HTML and would take that click to the image host
    // instead, so the image stays part of the surrounding link's own text.
    if (held && !target.closest?.("a")) {
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
    // Whatever is left of the image, its alt text is author prose worth keeping.
    const text = alt || held;
    if (text) target.appendChild(doc.createTextNode(text));
  }

  function appendSanitized(target, node, doc, baseURL, imageBaseURL, imageProxyURL) {
    if (node.nodeType === 3) {
      target.appendChild(doc.createTextNode(node.nodeValue));
      return;
    }
    if (node.nodeType !== 1) return;
    const tag = node.tagName.toUpperCase();
    if (DROPPED_ELEMENTS.has(tag)) return;
    if (!ALLOWED_ELEMENTS.has(tag)) {
      for (const child of Array.from(node.childNodes)) appendSanitized(target, child, doc, baseURL, imageBaseURL, imageProxyURL);
      return;
    }
    const clean = doc.createElement(tag.toLowerCase());
    if (copyAttributes(node, clean, tag, baseURL, imageBaseURL, imageProxyURL) === false) return;
    if (tag === "IMG" && !clean.hasAttribute("src")) {
      appendUnloadedImage(target, node, doc);
      return;
    }
    // The element joins the tree before its children are filled in, so a child
    // can tell what it is nested inside.
    target.appendChild(clean);
    for (const child of Array.from(node.childNodes)) appendSanitized(clean, child, doc, baseURL, imageBaseURL, imageProxyURL);
  }

  // options.baseURL is the page the markdown was written on. Relative images
  // prefer the authenticated same-origin proxy and fall back to imageBaseURL.
  function sanitize(html, ownerDocument, options) {
    const doc = ownerDocument || global.document;
    const baseURL = safeURL(options?.baseURL, SAFE_BASE_SCHEMES);
    const imageBaseURL = safeURL(options?.imageBaseURL, SAFE_BASE_SCHEMES);
    const imageProxyURL = safeURL(options?.imageProxyURL, SAFE_BASE_SCHEMES);
    const parsed = new DOMParser().parseFromString(String(html || ""), "text/html");
    const fragment = doc.createDocumentFragment();
    for (const child of Array.from(parsed.body.childNodes)) {
      appendSanitized(fragment, child, doc, baseURL, imageBaseURL, imageProxyURL);
    }
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
