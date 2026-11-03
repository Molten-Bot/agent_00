(function () {
  "use strict";

  const status = document.getElementById("github-issues-status");
  const disabled = document.getElementById("github-issues-disabled");
  const browser = document.getElementById("github-issues-browser");
  const repoSelect = document.getElementById("github-issues-repo");
  const searchInput = document.getElementById("github-issues-search");
  const filters = document.getElementById("github-issues-filters");
  const issueList = document.getElementById("github-issues-list");
  const issueCount = document.getElementById("github-issues-count");
  const detail = document.getElementById("github-issue-detail");
  const columns = document.querySelector(".github-issues-columns");
  const resizeHandle = document.getElementById("github-issues-resize-handle");
  if (!status || !disabled || !browser || !repoSelect || !filters || !issueList || !detail) return;

  const layoutStorageKey = "hubui.githubIssuesLayout.v1";
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

  function setStatus(message, kind) {
    status.textContent = String(message || "");
    status.dataset.tone = kind || "";
  }

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

  function formatPeople(people) {
    return (Array.isArray(people) ? people : []).map((person) => person?.login).filter(Boolean).join(", ") || "None";
  }

  function formatLabels(labels) {
    return (Array.isArray(labels) ? labels : []).map((label) => label?.name).filter(Boolean).join(", ") || "None";
  }

  function taskLinkModeDescription(issue) {
    return githubIssueLinkMode === "close"
      ? `Issue and comments included as context · PR will close #${issue.number} on merge`
      : `Issue and comments included as context · PR will reference #${issue.number}`;
  }

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

  function addIssueLabels(parent, labels) {
    for (const label of Array.isArray(labels) ? labels : []) {
      const name = String(label?.name || "").trim();
      if (name) addText(parent, "span", "github-issue-label", name);
    }
  }

  function addAvatar(parent, login) {
    const name = String(login || "?").trim();
    const initials = name.split(/[-_\s]+/).filter(Boolean).slice(0, 2).map((part) => part[0].toUpperCase()).join("") || "?";
    const avatar = addText(parent, "span", "github-issue-avatar", initials);
    avatar.setAttribute("aria-hidden", "true");
  }

  function buildTaskPrompt(issue, repo) {
    const repository = String(repo?.full_name || "").trim();
    const comments = Array.isArray(issue.comments) ? issue.comments : [];
    const context = [
      `Implement GitHub issue #${issue.number} in ${repository}. Work in the repository, inspect the existing code, implement the requested change, and produce a pull request. Treat the issue description and conversation below as the complete issue context.`,
      `Issue: ${issue.title || "Untitled issue"}`,
      `Issue URL: ${issue.url || ""}`,
      `State: ${issue.state || "unknown"}`,
      `Author: ${issue.author?.login || "unknown"}`,
      `Labels: ${formatLabels(issue.labels)}`,
      `Assignees: ${formatPeople(issue.assignees)}`,
      "",
      "Issue description:",
      String(issue.body || "(No description provided.)"),
      "",
      "Issue conversation:",
    ];
    if (!comments.length) context.push("(No comments.)");
    for (const comment of comments) {
      context.push(`\n${comment?.author?.login || "unknown"} · ${comment?.createdAt || ""}:`);
      context.push(String(comment?.body || ""));
    }
    return context.join("\n").trim();
  }

  function renderIssueList(issues) {
    issueList.replaceChildren();
    if (issueCount) issueCount.textContent = `${issues.length}${issues.length === 100 ? "+" : ""} open issues`;
    if (!issues.length) {
      addText(issueList, "li", "github-issues-empty", "No issues found for these filters.");
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
      addText(meta, "span", "", `updated ${formatRelativeTime(issue.updatedAt)}`);
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
    detail.appendChild(detailHeader);

    const detailBody = document.createElement("div");
    detailBody.className = "github-issue-detail-body";
    const author = addText(detailBody, "div", "github-issue-author", "");
    addAvatar(author, issue.author?.login);
    addText(author, "strong", "", issue.author?.login || "Unknown");
    addText(author, "span", "", `opened this issue ${formatRelativeTime(issue.createdAt)}`);
    addText(detailBody, "pre", "github-issue-body", issue.body || "No description provided.");
    const metadata = document.createElement("dl");
    metadata.className = "github-issue-metadata";
    for (const [label, value] of [
      ["Assignees", formatPeople(issue.assignees)],
      ["Created", formatRelativeTime(issue.createdAt)],
      ["Updated", formatRelativeTime(issue.updatedAt)],
    ]) {
      const group = document.createElement("div");
      addText(group, "dt", "", label);
      addText(group, "dd", "", value);
      metadata.appendChild(group);
    }
    detailBody.appendChild(metadata);
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
      addText(commentMeta, "span", "", formatRelativeTime(comment.createdAt));
      addText(card, "pre", "github-issue-comment-body", comment.body || "");
      comments.appendChild(card);
    }
    detailBody.appendChild(comments);

    const task = document.createElement("section");
    task.className = "github-issue-task";
    const taskHeading = addText(task, "div", "github-issue-task-heading", "");
    addText(taskHeading, "strong", "", "Start a task from this issue");
    addText(taskHeading, "span", "github-issue-link-mode-note", taskLinkModeDescription(issue));
    const editor = document.createElement("textarea");
    editor.className = "github-issue-prompt";
    editor.rows = 7;
    editor.setAttribute("aria-label", "Issue task prompt");
    editor.value = buildTaskPrompt(issue, repo);
    editor.hidden = true;
    task.appendChild(editor);
    const actions = document.createElement("div");
    actions.className = "prompt-actions-end github-issue-task-actions";
    const edit = document.createElement("button");
    edit.type = "button";
    edit.className = "prompt-action-button";
    edit.textContent = "Edit prompt first";
    edit.addEventListener("click", () => {
      editor.hidden = false;
      submitEdited.hidden = false;
      editor.focus();
    });
    const start = document.createElement("button");
    start.type = "button";
    start.className = "prompt-action-button prompt-submit github-issue-start";
    setStartButtonContent(start, "Start task");
    start.addEventListener("click", () => void submitTask(issue, repo, buildTaskPrompt(issue, repo)));
    const submitEdited = document.createElement("button");
    submitEdited.type = "button";
    submitEdited.className = "prompt-action-button prompt-submit github-issue-start";
    setStartButtonContent(submitEdited, "Start with this prompt");
    submitEdited.hidden = true;
    submitEdited.addEventListener("click", () => void submitTask(issue, repo, editor.value));
    actions.append(edit, start, submitEdited);
    task.appendChild(actions);
    const taskStatus = addText(task, "p", "github-issue-task-status", "");
    taskStatus.setAttribute("role", "status");
    taskStatus.setAttribute("aria-live", "polite");
    detailBody.appendChild(task);
    detail.appendChild(detailBody);
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
        headers: { "Content-Type": "application/json", "X-Agent-Task-Source": "prompt" },
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
    const repo = selectedRepository();
    selectedIssue = null;
    selectedIssueNumber = null;
    loadingIssueNumber = null;
    currentIssues = [];
    detail.replaceChildren();
    issueList.replaceChildren();
    if (!repo) {
      setStatus("Select a repository to browse its issues.");
      return;
    }
    const generation = ++requestGeneration;
    setStatus(`Loading issues for ${repo.full_name}…`);
    const query = new URLSearchParams({ repo: repo.full_name, state: "open" });
    if (searchInput.value.trim()) query.set("search", searchInput.value.trim());
    try {
      const body = await requestJSON(`/api/github/issues?${query}`, { cache: "no-store" });
      if (generation !== requestGeneration) return;
      const issues = Array.isArray(body.issues) ? body.issues : [];
      currentIssues = issues;
      renderIssueList(issues);
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
      setStatus(repositories.length ? "Select a repository to browse its issues." : "No repositories are available for this GitHub account.");
    } catch (error) {
      setStatus(error.message || "Unable to initialize the GitHub Issues browser.", "error");
    }
  }

  repoSelect.addEventListener("change", () => void loadIssues());
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
  if (document.body.dataset.promptMode === "issues") {
    void window.initGitHubIssuesPanel();
  }
})();
