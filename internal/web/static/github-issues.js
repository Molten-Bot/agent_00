(function () {
  "use strict";

  const status = document.getElementById("github-issues-status");
  const disabled = document.getElementById("github-issues-disabled");
  const browser = document.getElementById("github-issues-browser");
  const repoSelect = document.getElementById("github-issues-repo");
  const stateSelect = document.getElementById("github-issues-state");
  const searchInput = document.getElementById("github-issues-search");
  const filters = document.getElementById("github-issues-filters");
  const issueList = document.getElementById("github-issues-list");
  const detail = document.getElementById("github-issue-detail");
  if (!status || !disabled || !browser || !repoSelect || !filters || !issueList || !detail) return;

  let repositories = [];
  let currentIssues = [];
  let selectedIssue = null;
  let activeLinkMode = "reference";
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

  function issueTitle(issue) {
    return `#${issue.number} ${issue.title || "Untitled issue"}`;
  }

  function addText(parent, tag, className, text) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    node.textContent = String(text || "");
    parent.appendChild(node);
    return node;
  }

  function formatPeople(people) {
    return (Array.isArray(people) ? people : []).map((person) => person?.login).filter(Boolean).join(", ") || "None";
  }

  function formatLabels(labels) {
    return (Array.isArray(labels) ? labels : []).map((label) => label?.name).filter(Boolean).join(", ") || "None";
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
    if (!issues.length) {
      addText(issueList, "li", "github-issues-empty", "No issues found for these filters.");
      return;
    }
    for (const issue of issues) {
      const item = document.createElement("li");
      const button = document.createElement("button");
      button.type = "button";
      button.className = "github-issue-list-item";
      button.setAttribute("aria-current", String(selectedIssue?.number === issue.number));
      button.dataset.issueNumber = String(issue.number || "");
      button.dataset.title = String(issue.title || "");
      addText(button, "strong", "github-issue-list-title", issueTitle(issue));
      const summary = [issue.state, issue.author?.login ? `by ${issue.author.login}` : "", formatLabels(issue.labels)]
        .filter((part) => part && part !== "None").join(" · ");
      addText(button, "span", "github-issue-list-summary", summary);
      button.addEventListener("click", () => void loadIssue(issue.number));
      item.appendChild(button);
      issueList.appendChild(item);
    }
  }

  function renderIssueDetail(issue) {
    selectedIssue = issue;
    activeLinkMode = "reference";
    detail.replaceChildren();
    const repo = selectedRepository();
    addText(detail, "p", "github-issue-number", `${issue.state || "Issue"} · #${issue.number}`);
    addText(detail, "h2", "github-issue-title", issue.title || "Untitled issue");
    if (issue.url) {
      const link = document.createElement("a");
      link.href = issue.url;
      link.target = "_blank";
      link.rel = "noopener noreferrer";
      link.textContent = "Open on GitHub";
      detail.appendChild(link);
    }
    const metadata = document.createElement("dl");
    metadata.className = "github-issue-metadata";
    for (const [label, value] of [
      ["Author", issue.author?.login || "Unknown"],
      ["Assignees", formatPeople(issue.assignees)],
      ["Labels", formatLabels(issue.labels)],
      ["Updated", issue.updatedAt || "Unknown"],
    ]) {
      addText(metadata, "dt", "", label);
      addText(metadata, "dd", "", value);
    }
    detail.appendChild(metadata);
    addText(detail, "h3", "", "Description");
    addText(detail, "pre", "github-issue-body", issue.body || "No description provided.");
    addText(detail, "h3", "", `Conversation (${(issue.comments || []).length})`);
    const comments = document.createElement("div");
    comments.className = "github-issue-comments";
    if (!issue.comments?.length) addText(comments, "p", "", "No comments yet.");
    for (const comment of issue.comments || []) {
      const card = document.createElement("section");
      card.className = "github-issue-comment";
      addText(card, "p", "github-issue-comment-meta", `${comment.author?.login || "Unknown"} · ${comment.createdAt || ""}`);
      addText(card, "pre", "github-issue-comment-body", comment.body || "");
      comments.appendChild(card);
    }
    detail.appendChild(comments);

    const task = document.createElement("section");
    task.className = "github-issue-task";
    addText(task, "h3", "", "Start a task");
    addText(task, "p", "", "The issue context goes to the agent. The run then follows your regular pull request, review, and merge settings.");
    const linkLabel = document.createElement("label");
    linkLabel.className = "github-issue-link-mode";
    linkLabel.appendChild(document.createTextNode("PR issue link "));
    const linkMode = document.createElement("select");
    linkMode.setAttribute("aria-label", "PR issue link behavior");
    for (const [value, label] of [["reference", "Link only"], ["close", "Close issue when merged"]]) {
      const option = document.createElement("option");
      option.value = value;
      option.textContent = label;
      option.selected = value === activeLinkMode;
      linkMode.appendChild(option);
    }
    linkMode.addEventListener("change", () => { activeLinkMode = linkMode.value; });
    linkLabel.appendChild(linkMode);
    task.appendChild(linkLabel);
    const editor = document.createElement("textarea");
    editor.className = "github-issue-prompt";
    editor.rows = 12;
    editor.setAttribute("aria-label", "Issue task prompt");
    editor.value = buildTaskPrompt(issue, repo);
    editor.hidden = true;
    task.appendChild(editor);
    const actions = document.createElement("div");
    actions.className = "github-issue-task-actions";
    const start = document.createElement("button");
    start.type = "button";
    start.className = "github-issue-start";
    start.textContent = "Start task";
    start.addEventListener("click", () => void submitTask(issue, repo, buildTaskPrompt(issue, repo)));
    const edit = document.createElement("button");
    edit.type = "button";
    edit.textContent = "Edit prompt first";
    edit.addEventListener("click", () => {
      editor.hidden = false;
      edit.hidden = true;
      submitEdited.hidden = false;
      editor.focus();
    });
    const submitEdited = document.createElement("button");
    submitEdited.type = "button";
    submitEdited.className = "github-issue-start";
    submitEdited.textContent = "Start edited task";
    submitEdited.hidden = true;
    submitEdited.addEventListener("click", () => void submitTask(issue, repo, editor.value));
    actions.append(start, edit, submitEdited);
    task.appendChild(actions);
    const taskStatus = addText(task, "p", "github-issue-task-status", "");
    taskStatus.setAttribute("role", "status");
    taskStatus.setAttribute("aria-live", "polite");
    detail.appendChild(task);
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
            linkMode: activeLinkMode,
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
    setStatus(`Loading issue #${number}…`);
    try {
      const query = new URLSearchParams({ repo: repo.full_name });
      const body = await requestJSON(`/api/github/issues/${encodeURIComponent(number)}?${query}`, { cache: "no-store" });
      if (generation !== requestGeneration) return;
      renderIssueDetail(body.issue);
      renderIssueList(currentIssues);
      setStatus("");
    } catch (error) {
      if (generation === requestGeneration) setStatus(error.message || "Unable to load issue.", "error");
    }
  }

  async function loadIssues() {
    const repo = selectedRepository();
    selectedIssue = null;
    currentIssues = [];
    detail.replaceChildren();
    issueList.replaceChildren();
    if (!repo) {
      setStatus("Select a repository to browse its issues.");
      return;
    }
    const generation = ++requestGeneration;
    setStatus(`Loading issues for ${repo.full_name}…`);
    const query = new URLSearchParams({ repo: repo.full_name, state: stateSelect.value });
    if (searchInput.value.trim()) query.set("search", searchInput.value.trim());
    try {
      const body = await requestJSON(`/api/github/issues?${query}`, { cache: "no-store" });
      if (generation !== requestGeneration) return;
      const issues = Array.isArray(body.issues) ? body.issues : [];
      currentIssues = issues;
      renderIssueList(issues);
      setStatus(`${issues.length}${issues.length === 100 ? "+" : ""} issues`);
    } catch (error) {
      if (generation === requestGeneration) setStatus(error.message || "Unable to load issues.", "error");
    }
  }

  async function initialize() {
    try {
      const settings = await requestJSON("/api/review-settings", { cache: "no-store" });
      const enabled = Boolean(settings.settings?.github_issues_enabled ?? settings.settings?.githubIssuesEnabled);
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
  stateSelect.addEventListener("change", () => void loadIssues());
  filters.addEventListener("submit", (event) => {
    event.preventDefault();
    void loadIssues();
  });
  void initialize();
})();
