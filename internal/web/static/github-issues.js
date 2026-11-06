(function () {
  "use strict";

  const status = document.getElementById("github-issues-status");
  const disabled = document.getElementById("github-issues-disabled");
  const browser = document.getElementById("github-issues-browser");
  const repoSelect = document.getElementById("github-issues-repo");
  const searchInput = document.getElementById("github-issues-search");
  const assigneeFilter = document.getElementById("github-issues-assignee-filter");
  const filters = document.getElementById("github-issues-filters");
  const issueList = document.getElementById("github-issues-list");
  const issueCount = document.getElementById("github-issues-count");
  const detail = document.getElementById("github-issue-detail");
  const columns = document.querySelector(".github-issues-columns");
  const resizeHandle = document.getElementById("github-issues-resize-handle");
  if (!status || !disabled || !browser || !repoSelect || !searchInput || !filters || !issueList || !detail) return;

  const layoutStorageKey = "hubui.githubIssuesLayout.v1";
  const repositoryStorageKey = "hubui.githubIssuesRepository.v1";
  const assigneeStorageKey = "hubui.githubIssuesAssignee.v1";
  const assigneeFilterLabel = "Show only issues assigned to me";
  let savedLayout = {};
  try { savedLayout = JSON.parse(localStorage.getItem(layoutStorageKey) || "{}"); } catch (_error) { savedLayout = {}; }

  function saveLayout() {
    try {
      const width = columns ? parseFloat(getComputedStyle(columns).getPropertyValue("--github-issues-list-width")) : NaN;
      localStorage.setItem(layoutStorageKey, JSON.stringify({ width: Number.isFinite(width) ? width : null }));
    } catch (_error) { /* Storage can be unavailable in private or embedded contexts. */ }
  }

  function setListWidth(width) {
    if (!columns || !resizeHandle) return;
    const bounds = columns.getBoundingClientRect();
    const minimum = 220;
    const maximum = Math.max(minimum, Math.min(bounds.width * 0.6, bounds.width - 320));
    const next = Math.round(Math.max(minimum, Math.min(maximum, width)));
    columns.style.setProperty("--github-issues-list-width", `${next}px`);
    resizeHandle.setAttribute("aria-valuemax", String(Math.round(maximum)));
    resizeHandle.setAttribute("aria-valuenow", String(next));
  }

  if (columns) setListWidth(Number.isFinite(Number(savedLayout.width)) ? Number(savedLayout.width) : Math.max(220, columns.clientWidth * 0.32));

  if (columns && resizeHandle) {
    let dragStartX = 0;
    let dragStartWidth = 0;
    resizeHandle.addEventListener("pointerdown", (event) => {
      if (event.button !== 0 || window.matchMedia("(max-width: 680px)").matches) return;
      event.preventDefault();
      dragStartX = event.clientX;
      dragStartWidth = resizeHandle.getBoundingClientRect().left - columns.getBoundingClientRect().left;
      columns.classList.add("is-resizing");
      resizeHandle.setPointerCapture(event.pointerId);
    });
    resizeHandle.addEventListener("pointermove", (event) => {
      if (!columns.classList.contains("is-resizing")) return;
      setListWidth(dragStartWidth + event.clientX - dragStartX);
    });
    const stopResize = () => {
      if (!columns.classList.contains("is-resizing")) return;
      columns.classList.remove("is-resizing");
      saveLayout();
    };
    resizeHandle.addEventListener("pointerup", stopResize);
    resizeHandle.addEventListener("pointercancel", stopResize);
    resizeHandle.addEventListener("keydown", (event) => {
      if (event.key === "ArrowLeft" || event.key === "ArrowRight") {
        event.preventDefault();
        const current = parseFloat(getComputedStyle(columns).getPropertyValue("--github-issues-list-width")) || columns.clientWidth * 0.32;
        setListWidth(current + (event.key === "ArrowRight" ? 24 : -24));
        saveLayout();
      } else if (event.key === "Home") {
        event.preventDefault();
        setListWidth(220);
        saveLayout();
      } else if (event.key === "End") {
        event.preventDefault();
        setListWidth(columns.clientWidth * 0.6);
        saveLayout();
      }
    });
  }

  let repositories = [];
  let currentIssues = [];
  let selectedIssue = null;
  let selectedIssueNumber = null;
  let loadingIssueNumber = null;
  let githubIssueLinkMode = "reference";
  let requestGeneration = 0;
  let searchTimer = null;
  // Issues assigned to the signed-in account are the default view; the filter
  // button turns it off to browse every open issue in the repository.
  let assignedToMe = true;
  try {
    const rememberedAssignee = localStorage.getItem(assigneeStorageKey);
    if (rememberedAssignee !== null) assignedToMe = rememberedAssignee === "true";
  } catch (_error) { /* Storage can be unavailable in private or embedded contexts. */ }

  function setStatus(message, kind) {
    status.textContent = String(message || "");
    status.dataset.tone = kind || "";
  }

  function syncAssigneeFilter() {
    // Search runs inside the assignee filter, so the placeholder names the scope it searches.
    searchInput.placeholder = assignedToMe ? "Search issues assigned to you" : "Search this repository";
    if (!assigneeFilter) return;
    assigneeFilter.setAttribute("aria-pressed", String(assignedToMe));
    assigneeFilter.setAttribute("aria-label", assigneeFilterLabel);
    assigneeFilter.title = assigneeFilterLabel;
  }

  // Reconcile the server-rendered defaults with the stored preference now, while
  // #github-issues-browser is still hidden, so the button and the search placeholder
  // are never shown contradicting the filter that is actually applied.
  syncAssigneeFilter();

  async function requestJSON(url, options) {
    const response = await fetch(url, options || { cache: "no-store" });
    let body = null;
    try { body = await response.json(); } catch (_error) { body = null; }
    if (!response.ok || body?.ok === false) {
      throw new Error(String(body?.error || `GitHub request failed (${response.status})`));
    }
    return body || {};
  }

  function selectedRepository() {
    return repositories.find((repo) => repo.full_name === repoSelect.value) || null;
  }

  function addText(parent, tag, className, text) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    node.textContent = String(text || "");
    parent.appendChild(node);
    return node;
  }

  function setStartButtonContent(button, label) {
    const icon = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    icon.classList.add("prompt-action-icon");
    icon.setAttribute("viewBox", "0 0 24 24");
    icon.setAttribute("fill", "none");
    icon.setAttribute("stroke", "currentColor");
    icon.setAttribute("stroke-width", "2");
    icon.setAttribute("stroke-linecap", "round");
    icon.setAttribute("stroke-linejoin", "round");
    icon.setAttribute("aria-hidden", "true");
    const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
    path.setAttribute("d", "m5 5 14 7-14 7V5");
    icon.appendChild(path);
    const text = document.createElement("span");
    text.className = "prompt-action-label";
    text.textContent = label;
    button.append(icon, text);
  }

  function formatLabels(labels) {
    return (Array.isArray(labels) ? labels : []).map((label) => label?.name).filter(Boolean).join(", ") || "None";
  }

  function taskLinkModeDescription(issue) {
    return githubIssueLinkMode === "close"
      ? `Issue and comments included as context · PR will close #${issue.number} on merge`
      : `Issue and comments included as context · PR will reference #${issue.number}`;
  }

  // GitHub returns UTC timestamps; this resolves them against the browser's
  // clock and timezone so every time reads in the viewer's terms.
  function formatRelativeTime(value) {
    const timestamp = Date.parse(String(value || ""));
    if (!Number.isFinite(timestamp)) return String(value || "Unknown");
    const delta = Math.max(0, Date.now() - timestamp);
    const minutes = Math.floor(delta / 60000);
    if (minutes < 1) return "just now";
    if (minutes < 60) return `${minutes} minute${minutes === 1 ? "" : "s"} ago`;
    const hours = Math.floor(minutes / 60);
    if (hours < 24) return `${hours} hour${hours === 1 ? "" : "s"} ago`;
    const days = Math.floor(hours / 24);
    if (days < 7) return `${days} day${days === 1 ? "" : "s"} ago`;
    return new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric", year: "numeric" }).format(timestamp);
  }

  // Renders a timestamp as a <time> carrying the machine-readable instant.
  // Header, list, and comments all read relative, which is the form the issue
  // view already used and what a reader scanning it is after; the datetime
  // attribute keeps the exact instant available to anything that needs it.
  function addTimestamp(parent, value) {
    const timestamp = Date.parse(String(value || ""));
    // A <time> without a machine-readable datetime is invalid markup, so an
    // unparsable value degrades to a plain span rather than lying about itself.
    if (!Number.isFinite(timestamp)) return addText(parent, "span", "", formatRelativeTime(value));
    const node = addText(parent, "time", "", formatRelativeTime(value));
    node.dateTime = new Date(timestamp).toISOString();
    return node;
  }

  function addIssueLabels(parent, labels) {
    for (const label of Array.isArray(labels) ? labels : []) {
      const name = String(label?.name || "").trim();
      if (name) addText(parent, "span", "github-issue-label", name);
    }
  }

  // Pass a label when the avatar is the only mention of the person; without one
  // the login is already written next to it and the initials are decorative.
  // A native title tooltip never opens on touch, so the label rides on
  // data-label and the chip takes focus: style.css reveals it on hover, on
  // keyboard focus, and on the focus a tap leaves behind.
  function addAvatar(parent, login, label) {
    const name = String(login || "?").trim();
    const initials = name.split(/[-_\s]+/).filter(Boolean).slice(0, 2).map((part) => part[0].toUpperCase()).join("") || "?";
    const avatar = addText(parent, "span", "github-issue-avatar", initials);
    if (label) {
      avatar.setAttribute("role", "img");
      avatar.setAttribute("aria-label", label);
      avatar.dataset.label = label;
      // The name only exists while the chip is hovered or focused, so WCAG
      // 1.4.13 requires it to be reachable by keyboard and dismissable without
      // moving the pointer: the tab stop supplies the first, Escape the second.
      avatar.tabIndex = 0;
      avatar.addEventListener("keydown", (event) => {
        if (event.key !== "Escape") return;
        event.stopPropagation();
        avatar.blur();
      });
    } else {
      avatar.setAttribute("aria-hidden", "true");
    }
    return avatar;
  }

  function addAssignees(parent, assignees) {
    const people = (Array.isArray(assignees) ? assignees : []).filter((person) => person?.login);
    if (!people.length) {
      addText(parent, "span", "github-issue-assignee-empty", "Unassigned");
      return;
    }
    for (const person of people) {
      const login = String(person.login);
      const name = String(person.name || "").trim();
      addAvatar(parent, login, name ? `${login} (${name})` : login);
    }
  }

  function buildTaskPrompt(issue) {
    const comments = Array.isArray(issue.comments) ? issue.comments : [];
    const context = [
      `#${issue.number}: ${issue.title || "Untitled issue"}`,
      "",
      String(issue.body || "(No description provided.)"),
      "",
      "Comments:",
    ];
    if (!comments.length) context.push("(No comments.)");
    for (const comment of comments) {
      context.push(`\n${comment?.author?.login || "unknown"} · ${comment?.createdAt || ""}:`);
      context.push(String(comment?.body || ""));
    }
    return context.join("\n").trim();
  }

  function emptyIssueListHeading() {
    if (!repositories.length) return "No repositories available";
    if (!selectedRepository()) return "Choose a repository";
    if (searchInput.value.trim()) return "No matching issues";
    return assignedToMe ? "No assigned issues" : "No open issues";
  }

  function emptyIssueListText() {
    const searching = Boolean(searchInput.value.trim());
    if (!repositories.length) return "No repositories are available for this account.";
    if (!selectedRepository()) return "Choose a repository to see its open issues.";
    if (assignedToMe) {
      return searching
        ? "No open issues assigned to you match this search. Use the filter button to search every open issue."
        : "No open issues are assigned to you. Use the filter button to show every open issue.";
    }
    return searching ? "No open issues match this search." : "This repository has no open issues.";
  }

  function renderIssueList(issues) {
    issueList.replaceChildren();
    const total = `${issues.length}${issues.length === 100 ? "+" : ""}`;
    // 100 is the page limit, so a "100+" count is always plural.
    const noun = issues.length === 1 ? "open issue" : "open issues";
    if (issueCount) issueCount.textContent = !selectedRepository()
      ? "Choose a repository"
      : assignedToMe
        ? `${total} ${noun} assigned to you`
        : `${total} ${noun}`;
    if (!issues.length) {
      addText(issueList, "li", "github-issues-empty", emptyIssueListText());
      return;
    }
    for (const issue of issues) {
      const item = document.createElement("li");
      const button = document.createElement("button");
      button.type = "button";
      button.className = "github-issue-list-item";
      const selected = selectedIssueNumber === issue.number;
      button.setAttribute("aria-current", String(selected));
      button.setAttribute("aria-busy", String(loadingIssueNumber === issue.number));
      button.dataset.issueNumber = String(issue.number || "");
      button.dataset.title = String(issue.title || "");
      const title = addText(button, "strong", "github-issue-list-title", "");
      const isClosed = String(issue.state || "").toLowerCase() === "closed";
      addText(title, "span", isClosed ? "github-issue-closed-dot" : "github-issue-open-dot", isClosed ? "○" : "●");
      addText(title, "span", "", issue.title || "Untitled issue");
      if (loadingIssueNumber === issue.number) {
        const loading = addText(title, "span", "github-issue-loading task-icons-breathe", "");
        loading.setAttribute("role", "status");
        loading.setAttribute("aria-label", "Loading issue details");
        const icon = document.createElementNS("http://www.w3.org/2000/svg", "svg");
        icon.classList.add("task-action-glyph-loader");
        icon.setAttribute("viewBox", "0 0 24 24");
        icon.setAttribute("fill", "none");
        icon.setAttribute("stroke", "currentColor");
        icon.setAttribute("stroke-width", "2");
        icon.setAttribute("stroke-linecap", "round");
        icon.setAttribute("stroke-linejoin", "round");
        icon.setAttribute("aria-hidden", "true");
        const circle = document.createElementNS("http://www.w3.org/2000/svg", "circle");
        circle.setAttribute("cx", "12");
        circle.setAttribute("cy", "12");
        circle.setAttribute("r", "10");
        icon.appendChild(circle);
        const arc = document.createElementNS("http://www.w3.org/2000/svg", "path");
        arc.setAttribute("d", "M12 2a10 10 0 0 1 10 10");
        icon.appendChild(arc);
        loading.appendChild(icon);
      }
      const meta = addText(button, "span", "github-issue-list-meta", "");
      addText(meta, "span", "", `#${issue.number}`);
      addText(meta, "span", "", "·");
      addTimestamp(addText(meta, "span", "", "updated "), issue.updatedAt);
      addIssueLabels(meta, issue.labels);
      button.addEventListener("click", () => void loadIssue(issue.number));
      item.appendChild(button);
      issueList.appendChild(item);
    }
  }

  function renderIssueDetail(issue) {
    selectedIssue = issue;
    detail.replaceChildren();
    const repo = selectedRepository();
    const detailHeader = document.createElement("header");
    detailHeader.className = "github-issue-detail-header";
    const headingMeta = addText(detailHeader, "div", "github-issue-heading-meta", "");
    const state = String(issue.state || "open").toLowerCase();
    addText(headingMeta, "span", "github-issue-state", `● ${state === "closed" ? "Closed" : "Open"}`);
    addText(headingMeta, "span", "", repo?.full_name || "Repository");
    addText(headingMeta, "span", "", "·");
    addText(headingMeta, "span", "", `#${issue.number}`);
    addText(detailHeader, "h2", "github-issue-detail-title", issue.title || "Untitled issue");
    const labels = addText(detailHeader, "div", "github-issue-labels", "");
    addIssueLabels(labels, issue.labels);
    if (issue.url) {
      const link = document.createElement("a");
      link.className = "github-issue-link";
      link.href = issue.url;
      link.target = "_blank";
      link.rel = "noopener noreferrer";
      link.textContent = "Open on GitHub";
      detailHeader.appendChild(link);
    }
    // Assignees and timestamps answer "who owns this and is it still moving",
    // so they close the header instead of waiting below the description.
    const metadata = document.createElement("dl");
    metadata.className = "github-issue-metadata";
    const assigneeGroup = document.createElement("div");
    addText(assigneeGroup, "dt", "", "Assignees");
    addAssignees(addText(assigneeGroup, "dd", "github-issue-assignees", ""), issue.assignees);
    metadata.appendChild(assigneeGroup);
    for (const [label, value] of [["Created", issue.createdAt], ["Updated", issue.updatedAt]]) {
      const group = document.createElement("div");
      addText(group, "dt", "", label);
      addTimestamp(addText(group, "dd", "", ""), value);
      metadata.appendChild(group);
    }
    detailHeader.appendChild(metadata);
    detail.appendChild(detailHeader);

    const detailBody = document.createElement("div");
    detailBody.className = "github-issue-detail-body";
    const author = addText(detailBody, "div", "github-issue-author", "");
    addAvatar(author, issue.author?.login);
    addText(author, "strong", "", issue.author?.login || "Unknown");
    // The header already carries Created, so the byline does not repeat it.
    addText(author, "span", "", "opened this issue");
    addText(detailBody, "pre", "github-issue-body", issue.body || "No description provided.");
    addText(detailBody, "h3", "github-issue-comments-heading", `Comments (${(issue.comments || []).length})`);
    const comments = document.createElement("div");
    comments.className = "github-issue-comments";
    if (!issue.comments?.length) addText(comments, "p", "github-issue-comment-body", "No comments yet.");
    for (const comment of issue.comments || []) {
      const card = document.createElement("section");
      card.className = "github-issue-comment";
      const commentMeta = addText(card, "div", "github-issue-comment-meta", "");
      addAvatar(commentMeta, comment.author?.login);
      addText(commentMeta, "strong", "", comment.author?.login || "Unknown");
      addText(commentMeta, "span", "", "·");
      addTimestamp(commentMeta, comment.createdAt);
      addText(card, "pre", "github-issue-comment-body", comment.body || "");
      comments.appendChild(card);
    }
    detailBody.appendChild(comments);

    const task = document.createElement("section");
    task.className = "github-issue-task";
    const taskHeading = addText(task, "div", "github-issue-task-heading", "");
    addText(taskHeading, "strong", "", "Start a task from this issue");
    addText(taskHeading, "span", "github-issue-link-mode-note", taskLinkModeDescription(issue));
    const actions = document.createElement("div");
    actions.className = "prompt-actions github-issue-task-actions";
    const taskStatus = addText(actions, "p", "submit-status submit-status-inline is-visible github-issue-task-status", "");
    taskStatus.setAttribute("role", "status");
    taskStatus.setAttribute("aria-live", "polite");
    const actionEnd = document.createElement("div");
    actionEnd.className = "prompt-actions-end";
    const start = document.createElement("button");
    start.type = "button";
    start.className = "prompt-action-button prompt-submit github-issue-start";
    setStartButtonContent(start, "Start task");
    start.addEventListener("click", () => void submitTask(issue, repo, buildTaskPrompt(issue)));
    actionEnd.append(start);
    actions.append(actionEnd);
    task.appendChild(actions);
    detailBody.appendChild(task);
    detail.appendChild(detailBody);
  }

  function renderEmptyDetail(title, description) {
    detail.replaceChildren();
    const empty = document.createElement("div");
    empty.className = "github-issue-empty-state";
    const icon = document.createElement("span");
    icon.className = "github-issue-empty-icon";
    icon.setAttribute("aria-hidden", "true");
    const glyph = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    glyph.setAttribute("viewBox", "0 0 24 24");
    glyph.setAttribute("fill", "none");
    glyph.setAttribute("stroke", "currentColor");
    glyph.setAttribute("stroke-width", "1.8");
    glyph.setAttribute("stroke-linecap", "round");
    glyph.setAttribute("stroke-linejoin", "round");
    const ring = document.createElementNS("http://www.w3.org/2000/svg", "circle");
    ring.setAttribute("cx", "12");
    ring.setAttribute("cy", "12");
    ring.setAttribute("r", "9");
    const mark = document.createElementNS("http://www.w3.org/2000/svg", "path");
    mark.setAttribute("d", "M8 12h8");
    glyph.append(ring, mark);
    icon.appendChild(glyph);
    empty.appendChild(icon);
    addText(empty, "h2", "", title);
    addText(empty, "p", "", description);
    detail.appendChild(empty);
  }

  async function submitTask(issue, repo, prompt) {
    const submitButtons = Array.from(detail.querySelectorAll(".github-issue-task-actions button"));
    const taskStatus = detail.querySelector(".github-issue-task-status");
    const repoName = String(repo?.full_name || "").trim();
    const repoURL = String(repo?.html_url || (repoName ? `https://github.com/${repoName}` : "")).trim();
    if (!repoName || !repoURL || !String(prompt || "").trim()) {
      if (taskStatus) taskStatus.textContent = "Repository and task prompt are required.";
      return;
    }
    submitButtons.forEach((button) => { button.disabled = true; });
    if (taskStatus) taskStatus.textContent = "Submitting task…";
    try {
      const body = await requestJSON("/api/local-prompt", {
        method: "POST",
        headers: { "Content-Type": "application/json", "X-Agent-Task-Source": "github_issues" },
        body: JSON.stringify({
          repo: repoURL,
          baseBranch: String(repo.default_branch || "").trim(),
          prompt: String(prompt).trim(),
          githubIssue: {
            repository: repoName,
            number: issue.number,
            url: issue.url,
            linkMode: githubIssueLinkMode,
          },
        }),
      });
      if (taskStatus) taskStatus.textContent = `Task queued (${body.request_id || "request accepted"}).`;
    } catch (error) {
      if (taskStatus) taskStatus.textContent = `Could not start task: ${error.message || "request failed"}`;
      submitButtons.forEach((button) => { button.disabled = false; });
    }
  }

  async function loadIssue(number) {
    const repo = selectedRepository();
    if (!repo) return;
    const generation = ++requestGeneration;
    selectedIssueNumber = number;
    loadingIssueNumber = number;
    renderIssueList(currentIssues);
    setStatus(`Loading issue #${number}…`);
    try {
      const query = new URLSearchParams({ repo: repo.full_name });
      const body = await requestJSON(`/api/github/issues/${encodeURIComponent(number)}?${query}`, { cache: "no-store" });
      if (generation !== requestGeneration) return;
      loadingIssueNumber = null;
      renderIssueDetail(body.issue);
      renderIssueList(currentIssues);
      setStatus("");
    } catch (error) {
      if (generation === requestGeneration) {
        loadingIssueNumber = null;
        renderIssueList(currentIssues);
        setStatus(error.message || "Unable to load issue.", "error");
      }
    }
  }

  async function loadIssues() {
    if (searchTimer !== null) {
      window.clearTimeout(searchTimer);
      searchTimer = null;
    }
    const repo = selectedRepository();
    selectedIssue = null;
    selectedIssueNumber = null;
    loadingIssueNumber = null;
    currentIssues = [];
    detail.replaceChildren();
    issueList.replaceChildren();
    if (!repo) {
      renderIssueList([]);
      renderEmptyDetail("Choose a repository", "Select a repository above to browse its open issues.");
      setStatus("");
      return;
    }
    renderEmptyDetail("Choose an issue", "Select an issue to review its description, comments, and task options.");
    const generation = ++requestGeneration;
    setStatus(`Loading issues for ${repo.full_name}…`);
    const query = new URLSearchParams({ repo: repo.full_name, state: "open" });
    if (searchInput.value.trim()) query.set("search", searchInput.value.trim());
    if (assignedToMe) query.set("assignee", "me");
    try {
      const body = await requestJSON(`/api/github/issues?${query}`, { cache: "no-store" });
      if (generation !== requestGeneration) return;
      const issues = Array.isArray(body.issues) ? body.issues : [];
      currentIssues = issues;
      renderIssueList(issues);
      // There is nothing to choose when the list came back empty, so the detail
      // pane repeats the list's reason instead of inviting a selection.
      if (!issues.length) renderEmptyDetail(emptyIssueListHeading(), emptyIssueListText());
      setStatus("");
    } catch (error) {
      if (generation === requestGeneration) setStatus(error.message || "Unable to load issues.", "error");
    }
  }

  async function initialize() {
    try {
      const settings = await requestJSON("/api/review-settings", { cache: "no-store" });
      const values = settings.settings || {};
      const enabled = Boolean(values.github_issues_enabled ?? values.githubIssuesEnabled);
      const linkMode = String(values.github_issue_link_mode ?? values.githubIssueLinkMode ?? "reference").trim().toLowerCase();
      githubIssueLinkMode = linkMode === "close" ? "close" : "reference";
      if (!enabled) {
        disabled.hidden = false;
        setStatus("");
        return;
      }
      browser.hidden = false;
      const response = await requestJSON("/api/github/repos", { cache: "no-store" });
      repositories = Array.isArray(response.repos) ? response.repos : [];
      repoSelect.replaceChildren();
      const placeholder = document.createElement("option");
      placeholder.value = "";
      placeholder.textContent = repositories.length ? "Select a repository" : "No repositories available";
      repoSelect.appendChild(placeholder);
      for (const repo of repositories) {
        const option = document.createElement("option");
        option.value = String(repo.full_name || "");
        option.textContent = String(repo.full_name || repo.name || "");
        repoSelect.appendChild(option);
      }
      if (!repositories.length) {
        renderIssueList([]);
        renderEmptyDetail("No repositories available", "This GitHub account has no repositories you can browse.");
        setStatus("");
        return;
      }
      const promptRepository = document.getElementById("builder-repo-select")?.value || "";
      let rememberedRepository = "";
      try { rememberedRepository = localStorage.getItem(repositoryStorageKey) || ""; } catch (_error) { /* Ignore unavailable storage. */ }
      const preferred = repositories.find((repo) => repo.full_name === promptRepository)
        || repositories.find((repo) => repo.full_name === rememberedRepository);
      if (preferred) {
        repoSelect.value = preferred.full_name;
        try { localStorage.setItem(repositoryStorageKey, preferred.full_name); } catch (_error) { /* Ignore unavailable storage. */ }
        void loadIssues();
      } else {
        renderIssueList([]);
        renderEmptyDetail("Choose a repository", "Select a repository above to browse its open issues.");
        setStatus("");
      }
    } catch (error) {
      setStatus(error.message || "Unable to initialize the GitHub Issues browser.", "error");
    }
  }

  repoSelect.addEventListener("change", () => {
    try {
      if (repoSelect.value) localStorage.setItem(repositoryStorageKey, repoSelect.value);
      else localStorage.removeItem(repositoryStorageKey);
    } catch (_error) { /* Ignore unavailable storage. */ }
    void loadIssues();
  });
  if (assigneeFilter) {
    assigneeFilter.addEventListener("click", () => {
      assignedToMe = !assignedToMe;
      try { localStorage.setItem(assigneeStorageKey, String(assignedToMe)); } catch (_error) { /* Ignore unavailable storage. */ }
      syncAssigneeFilter();
      void loadIssues();
    });
  }
  searchInput.addEventListener("input", () => {
    if (searchTimer !== null) window.clearTimeout(searchTimer);
    searchTimer = window.setTimeout(() => {
      searchTimer = null;
      void loadIssues();
    }, 250);
  });
  filters.addEventListener("submit", (event) => {
    event.preventDefault();
    void loadIssues();
  });
  window.addEventListener("github-issue-link-mode-updated", (event) => {
    const mode = String(event.detail?.mode || "reference").trim().toLowerCase();
    githubIssueLinkMode = mode === "close" ? "close" : "reference";
    if (selectedIssue) {
      const note = detail.querySelector(".github-issue-link-mode-note");
      if (note) note.textContent = taskLinkModeDescription(selectedIssue);
    }
  });
  let initialization = null;
  window.initGitHubIssuesPanel = function initGitHubIssuesPanel() {
    if (!initialization) initialization = initialize();
    return initialization;
  };
  void window.initGitHubIssuesPanel();
})();
