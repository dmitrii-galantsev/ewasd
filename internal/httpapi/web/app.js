(() => {
  "use strict";

  const query = new URLSearchParams(window.location.search);
  if (query.has("token")) {
    sessionStorage.setItem("ewasd-token", query.get("token"));
    history.replaceState({}, "", window.location.pathname + window.location.hash);
  }

  const state = {
    snapshot: null,
    selectedProject: localStorage.getItem("ewasd-project") || null,
    view: "projects",
    token: sessionStorage.getItem("ewasd-token") || "",
    plan: null,
    planPath: "",
  };

  const main = document.querySelector("#main-content");
  const registerDialog = document.querySelector("#register-dialog");
  const entryDialog = document.querySelector("#entry-dialog");
  const planDialog = document.querySelector("#plan-dialog");
  const applyButton = document.querySelector("#apply-plan");

  const escapeHTML = (value = "") => String(value)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#039;");

  async function api(path, options = {}) {
    const headers = { "Accept": "application/json", ...(options.headers || {}) };
    if (options.body) headers["Content-Type"] = "application/json";
    if (state.token) headers.Authorization = `Bearer ${state.token}`;
    const response = await fetch(path, { ...options, headers });
    let payload;
    try {
      payload = await response.json();
    } catch {
      throw new Error(`The local engine returned HTTP ${response.status} without JSON.`);
    }
    if (!response.ok) {
      const error = new Error(payload.detail || `Request failed with HTTP ${response.status}`);
      error.recover = payload.recover || "Refresh and try again.";
      error.code = payload.error;
      throw error;
    }
    return payload;
  }

  async function refresh({ quiet = false } = {}) {
    if (!quiet) main.setAttribute("aria-busy", "true");
    try {
      state.snapshot = await api("/api/v1/snapshot");
      const projects = state.snapshot.projects;
      if (!projects.some(project => project.id === state.selectedProject)) {
        state.selectedProject = projects[0]?.id || null;
      }
      if (state.selectedProject) localStorage.setItem("ewasd-project", state.selectedProject);
      render();
    } catch (error) {
      if (error.code === "busy") {
        main.innerHTML = '<div class="loading-state" aria-live="polite"><span class="loader" aria-hidden="true"></span>An operation still holds the state lock. Retrying…</div>';
        setTimeout(() => refresh({ quiet: true }), 600);
        return;
      }
      renderFatal(error);
    } finally {
      main.removeAttribute("aria-busy");
    }
  }

  function selectedProject() {
    return state.snapshot?.projects.find(project => project.id === state.selectedProject) || null;
  }

  function render() {
    const snapshot = state.snapshot;
    document.querySelector("#project-count").textContent = snapshot.projects.length;
    document.querySelector("#revision-pill").textContent = `revision ${snapshot.revision}`;
    document.querySelector("#data-root-label").textContent = snapshot.data_root;
    document.querySelector("#recovery-dot").classList.toggle("is-hidden", snapshot.recovery.length === 0);
    renderRail();
    syncNavigation();

    if (state.view === "activity") {
      setTitle("Operation history", "Activity");
      renderActivity();
      return;
    }
    if (state.view === "safety") {
      setTitle("Recovery & guardrails", "Safety model");
      renderSafety();
      return;
    }
    const project = selectedProject();
    if (!project) {
      setTitle("Control deck", "Repository overlays");
      renderBlank();
      return;
    }
    setTitle(project.name, "Registered repository");
    renderProject(project);
  }

  function renderRail() {
    const rail = document.querySelector("#project-rail");
    rail.innerHTML = state.snapshot.projects.map(project => {
      const attention = project.health.conflicts + project.health.missing + project.health.source_missing > 0 || !project.git_ignore_ok;
      return `<button class="rail-project ${project.id === state.selectedProject ? "is-active" : ""}" type="button" data-project="${escapeHTML(project.id)}">
        <span class="repo-avatar">${escapeHTML(initials(project.name))}</span>
        <span><strong>${escapeHTML(project.name)}</strong><small>${escapeHTML(project.remote || "local checkout")}</small></span>
        <span class="rail-health ${attention ? "attention" : ""}" role="img" aria-label="${attention ? "Needs attention" : "Healthy"}"></span>
      </button>`;
    }).join("");
    rail.querySelectorAll("[data-project]").forEach(button => button.addEventListener("click", () => {
      state.selectedProject = button.dataset.project;
      state.view = "projects";
      localStorage.setItem("ewasd-project", state.selectedProject);
      render();
    }));
  }

  function renderBlank() {
    main.innerHTML = `<section class="blank-slate">
      <div class="blank-card">
        <span class="brand-mark" aria-hidden="true"><i></i><i></i><i></i></span>
        <p class="eyebrow">Start with identity, not inference</p>
        <h2>Register one checkout explicitly.</h2>
        <p>Nothing is scanned or moved during registration. Afterward, each adoption gets a read-only plan and an exact state revision.</p>
        <div class="guardrail-list" aria-label="Built-in guardrails">
          <div class="guardrail"><strong>No basename guessing</strong><span>The canonical checkout is recorded once.</span></div>
          <div class="guardrail"><strong>No silent overwrite</strong><span>Conflicts stop at the planning boundary.</span></div>
          <div class="guardrail"><strong>No source deletion</strong><span>Detach materializes and archives.</span></div>
        </div>
        <button class="button button-primary" id="blank-register" type="button">Register first checkout</button>
      </div>
    </section>`;
    document.querySelector("#blank-register").addEventListener("click", openRegister);
  }

  function renderProject(project) {
    const health = project.health;
    const issues = health.conflicts + health.source_missing;
    main.innerHTML = `<div class="view-stack">
      <section class="hero-card" aria-labelledby="project-heading">
        <div class="hero-copy">
          <p class="eyebrow">Explicit overlay manifest</p>
          <h2 id="project-heading">${escapeHTML(project.name)}</h2>
          <p>${health.total === 0 ? "Registered and intentionally empty. Adopt one path when you're ready; this engine won't infer a desired tree." : `${health.linked} of ${health.total} exact destinations currently resolve to their recorded sources.`}</p>
        </div>
        <div class="hero-meta">
          <div class="meta-row"><span>Checkout</span><code title="${escapeHTML(project.root)}">${escapeHTML(project.root)}</code></div>
          <div class="meta-row"><span>Identity</span><code title="${escapeHTML(project.remote || "No origin remote")}">${escapeHTML(project.remote || "No origin remote")}</code></div>
          <div class="meta-row"><span>Manifest</span><code>${escapeHTML(project.id)}</code></div>
          <div class="meta-row"><span>Git ignore</span><code>${escapeHTML(project.git_ignore_ok ? "Private block verified" : project.git_ignore_state)}</code></div>
        </div>
      </section>

      <section class="metric-grid" aria-label="Repository health">
        <div class="metric-card emphasis"><span>Linked</span><strong>${health.linked}</strong><small>verified ownership</small></div>
        <div class="metric-card ${health.missing ? "warning" : ""}"><span>Missing</span><strong>${health.missing}</strong><small>safe to reconcile</small></div>
        <div class="metric-card ${issues ? "warning" : ""}"><span>Conflicts</span><strong>${issues}</strong><small>never overwritten</small></div>
        <div class="metric-card"><span>Tracked</span><strong>${health.total}</strong><small>explicit entries</small></div>
      </section>

      <section class="panel" aria-labelledby="entries-heading">
        <div class="panel-head">
          <div><h3 id="entries-heading">Managed entries</h3><p>Manifest-owned destinations only. Filesystem discovery is intentionally absent.</p></div>
          <div class="panel-actions">
            <button class="button button-secondary" id="reconcile-button" type="button" ${health.missing === 0 && project.git_ignore_ok ? "disabled" : ""}>Reconcile ${health.missing || ""}</button>
            <button class="button button-primary" id="adopt-button" type="button">Adopt path</button>
          </div>
        </div>
        ${project.entries_view.length ? `<div class="entries">${project.entries_view.map(renderEntry).join("")}</div>` : `<div class="empty-panel"><div><span class="empty-symbol" aria-hidden="true">+</span><h3>No inferred files</h3><p>This is healthy. Add only a path you intentionally want this manifest to own.</p><button class="button button-primary" id="empty-adopt" type="button">Preview first adoption</button> <button class="button button-quiet" id="unregister-project" type="button">Unregister empty checkout</button></div></div>`}
      </section>
    </div>`;
    document.querySelector("#adopt-button")?.addEventListener("click", openEntry);
    document.querySelector("#empty-adopt")?.addEventListener("click", openEntry);
    document.querySelector("#unregister-project")?.addEventListener("click", unregisterProject);
    document.querySelector("#reconcile-button")?.addEventListener("click", () => requestPlan("reconcile", ""));
    main.querySelectorAll("[data-detach]").forEach(button => button.addEventListener("click", () => requestPlan("detach", button.dataset.detach)));
  }

  function renderEntry(entry) {
    return `<article class="entry-row">
      <div class="entry-name"><strong>${escapeHTML(entry.path)}</strong><code title="${escapeHTML(entry.source)}">${escapeHTML(entry.kind)} · ${escapeHTML(entry.source)}</code></div>
      <span class="status-badge ${escapeHTML(entry.status)}">${escapeHTML(entry.status.replace("-", " "))}</span>
      <div class="entry-detail">${escapeHTML(entry.detail)}</div>
      <div class="entry-actions"><button class="button button-danger" type="button" data-detach="${escapeHTML(entry.path)}" ${entry.status !== "linked" ? "disabled" : ""}>Detach</button></div>
    </article>`;
  }

  function renderActivity() {
    const items = state.snapshot.activity;
    main.innerHTML = `<div class="view-stack">
      <div class="section-heading"><div><p class="eyebrow">Bounded operation log</p><h2>What changed, in order.</h2><p>This isn't inferred from Git or the current tree. Every record was written with the state manifest.</p></div></div>
      <section class="panel" aria-label="Activity history">
        ${items.length ? `<div class="activity-list">${items.map(event => `<article class="activity-item">
          <span class="activity-icon" aria-hidden="true">${escapeHTML(event.action.slice(0,1))}</span>
          <div class="activity-copy"><strong>${escapeHTML(event.summary)}</strong><span>${escapeHTML(event.path || event.action)}${projectName(event.project_id) ? ` · ${escapeHTML(projectName(event.project_id))}` : ""}</span></div>
          <time datetime="${escapeHTML(event.created_at)}">${escapeHTML(formatTime(event.created_at))}</time>
        </article>`).join("")}</div>` : `<div class="empty-panel"><div><span class="empty-symbol" aria-hidden="true">—</span><h3>No operations recorded</h3><p>Registration and applied plans appear here. Read-only previews do not create history.</p></div></div>`}
      </section>
    </div>`;
  }

  function renderSafety() {
    const recovery = state.snapshot.recovery;
    main.innerHTML = `<div class="view-stack">
      <div class="section-heading"><div><p class="eyebrow">Designed failure boundaries</p><h2>Recovery is a first-class state.</h2><p>The engine doesn't claim cross-filesystem atomicity. It uses preconditions, atomic sibling renames, a synced manifest, and durable journals.</p></div></div>
      <div class="safety-grid">
        <section class="panel principles" aria-label="Safety principles">
          ${[
            ["01", "Exact ownership", "Only manifest entries can be reconciled or detached."],
            ["02", "Revision gate", "A stale plan can't apply after another operation changes state."],
            ["03", "Conflict means stop", "Normal files and foreign links are never force-overwritten."],
            ["04", "Retained content", "Adoption copies first; detach archives instead of deleting."],
          ].map(item => `<article class="principle"><span class="principle-index">${item[0]}</span><h3>${item[1]}</h3><p>${item[2]}</p></article>`).join("")}
        </section>
        <section class="panel recovery-panel" aria-labelledby="recovery-heading">
          <p class="eyebrow">Interrupted work</p><h3 id="recovery-heading">${recovery.length ? `${recovery.length} journal${recovery.length === 1 ? "" : "s"} need review` : "No recovery pending"}</h3>
          <p>${recovery.length ? "Recovery will complete committed cleanup or restore the pre-operation destination. Uncertain copies are retained." : "The transactions directory is empty. Completed operations removed their temporary backups and journals."}</p>
          ${recovery.map(item => `<div class="recovery-card"><strong>${escapeHTML(item.action)} · ${escapeHTML(item.phase)}</strong><code>${escapeHTML(item.path)} · ${escapeHTML(item.id)}</code><button class="button button-quiet" type="button" data-discard-journal="${escapeHTML(item.id)}">Archive journal only</button></div>`).join("")}
          <button class="button ${recovery.length ? "button-primary" : "button-quiet"}" id="recover-button" type="button" ${recovery.length ? "" : "disabled"}>Run conservative recovery</button>
        </section>
      </div>
    </div>`;
    document.querySelector("#recover-button")?.addEventListener("click", runRecovery);
    main.querySelectorAll("[data-discard-journal]").forEach(button => button.addEventListener("click", () => discardJournal(button.dataset.discardJournal)));
  }

  function openRegister() {
    clearError("register-error");
    registerDialog.showModal();
    setTimeout(() => document.querySelector("#register-root").focus(), 0);
  }

  function openEntry() {
    clearError("entry-error");
    document.querySelector("#entry-form").reset();
    entryDialog.showModal();
    setTimeout(() => document.querySelector("#entry-path").focus(), 0);
  }

  async function requestPlan(action, path) {
    const project = selectedProject();
    if (!project) return;
    try {
      const plan = await api(`/api/v1/projects/${encodeURIComponent(project.id)}/plans`, {
        method: "POST",
        body: JSON.stringify({ action, path, revision: state.snapshot.revision }),
      });
      entryDialog.close();
      showPlan(plan, path);
    } catch (error) {
      if (entryDialog.open) showError("entry-error", error);
      else toast(error.message, true);
    }
  }

  function showPlan(plan, path) {
    state.plan = plan;
    state.planPath = path;
    document.querySelector("#plan-eyebrow").textContent = `${plan.action} · revision ${plan.expected_revision}`;
    document.querySelector("#plan-title").textContent = !plan.safe
      ? "Plan stopped safely"
      : plan.steps.length === 0
        ? "No changes needed"
        : "Review before applying";
    document.querySelector("#plan-content").innerHTML = `
      <div class="plan-summary ${plan.safe ? "" : "blocked"}">${escapeHTML(plan.summary)}</div>
      <span class="plan-revision">expected revision ${plan.expected_revision}</span>
      ${plan.steps.length ? `<div class="plan-list">${plan.steps.map((step, index) => `<div class="plan-step"><span class="plan-step-index">${index + 1}</span><div><strong>${escapeHTML(step.action)} · ${escapeHTML(step.path)}</strong><span>${escapeHTML(step.detail)}</span></div></div>`).join("")}</div>` : ""}
      ${plan.conflicts.length ? `<div class="conflict-list">${plan.conflicts.map(item => `<div class="conflict-item"><strong>${escapeHTML(item.path || "Plan")}</strong><br>${escapeHTML(item.detail)}</div>`).join("")}</div>` : ""}
      <ul class="guarantee-list">${plan.guarantees.map(item => `<li>${escapeHTML(item)}</li>`).join("")}</ul>`;
    clearError("plan-error");
    applyButton.disabled = !plan.safe || plan.steps.length === 0;
    applyButton.textContent = !plan.safe
      ? "Blocked — no write available"
      : plan.steps.length === 0
        ? "No write needed"
        : `Apply ${plan.action}`;
    planDialog.showModal();
  }

  async function applyPlan() {
    const plan = state.plan;
    if (!plan) return;
    applyButton.disabled = true;
    applyButton.textContent = "Applying…";
    try {
      const result = await api(`/api/v1/projects/${encodeURIComponent(plan.project_id)}/apply`, {
        method: "POST",
        body: JSON.stringify({ action: plan.action, path: state.planPath, plan_id: plan.id, revision: plan.expected_revision }),
      });
      planDialog.close();
      toast(result.summary || "Plan applied safely.");
      await refresh({ quiet: true });
    } catch (error) {
      showError("plan-error", error);
      state.plan = null;
      applyButton.disabled = true;
      applyButton.textContent = "Close and preview again";
      await refresh({ quiet: true });
    }
  }

  async function runRecovery() {
    const button = document.querySelector("#recover-button");
    button.disabled = true;
    button.textContent = "Recovering…";
    try {
      const result = await api("/api/v1/recover", { method: "POST", body: "{}" });
      toast(result.messages.length ? result.messages.join(" · ") : "No recovery was needed.");
      await refresh({ quiet: true });
    } catch (error) {
      toast(`${error.message} ${error.recover || ""}`, true);
      button.disabled = false;
      button.textContent = "Run conservative recovery";
    }
  }

  async function discardJournal(id) {
    const confirmed = window.confirm("Archive this journal without changing any source, target, backup, or archive path? Inspect the filesystem first. This only removes the global write blocker.");
    if (!confirmed) return;
    try {
      const result = await api("/api/v1/recovery/discard", { method: "POST", body: JSON.stringify({ id, confirm: true }) });
      toast(`Journal archived at ${result.archive}. Files were not changed.`);
      await refresh({ quiet: true });
    } catch (error) {
      toast(`${error.message} ${error.recover || ""}`, true);
    }
  }

  async function unregisterProject() {
    const project = selectedProject();
    if (!project) return;
    if (!window.confirm(`Unregister ${project.name}? This is allowed only while it has no managed or unowned source files.`)) return;
    try {
      const result = await api(`/api/v1/projects/${encodeURIComponent(project.id)}/unregister`, { method: "POST", body: JSON.stringify({ action: "unregister", path: "", revision: state.snapshot.revision, confirm: true }) });
      state.selectedProject = null;
      toast(result.summary);
      await refresh({ quiet: true });
    } catch (error) {
      toast(`${error.message} ${error.recover || ""}`, true);
    }
  }

  function setTitle(title, eyebrow) {
    document.querySelector("#page-title").textContent = title;
    document.querySelector("#page-eyebrow").textContent = eyebrow;
    document.title = `${title} · ewasd`;
  }

  function syncNavigation() {
    document.querySelectorAll("[data-view]").forEach(button => button.classList.toggle("is-active", button.dataset.view === state.view));
  }

  function initials(name) {
    return name.split(/\s+/).filter(Boolean).slice(0, 2).map(part => part[0]).join("") || "R";
  }

  function projectName(id) {
    return state.snapshot.projects.find(project => project.id === id)?.name || "";
  }

  function formatTime(value) {
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return value;
    return new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" }).format(date);
  }

  function showError(id, error) {
    const node = document.querySelector(`#${id}`);
    node.textContent = `${error.message}${error.recover ? ` ${error.recover}` : ""}`;
    node.classList.remove("is-hidden");
    node.focus?.();
  }

  function clearError(id) {
    const node = document.querySelector(`#${id}`);
    node.textContent = "";
    node.classList.add("is-hidden");
  }

  function toast(message, isError = false) {
    const node = document.createElement("div");
    node.className = `toast${isError ? " error" : ""}`;
    node.textContent = message;
    document.querySelector("#toast-stack").append(node);
    setTimeout(() => node.remove(), 5200);
  }

  function renderFatal(error) {
    setTitle("Engine unavailable", "Connection error");
    main.innerHTML = `<section class="blank-slate"><div class="blank-card"><span class="empty-symbol" aria-hidden="true">!</span><h2>Couldn't read local state.</h2><p>${escapeHTML(error.message)} ${escapeHTML(error.recover || "If this console uses a token, reopen its pairing URL.")}</p><button class="button button-primary" id="retry-load" type="button">Retry connection</button></div></section>`;
    document.querySelector("#retry-load").addEventListener("click", () => refresh());
  }

  document.querySelectorAll("[data-view]").forEach(button => button.addEventListener("click", () => {
    state.view = button.dataset.view;
    render();
    main.focus();
  }));
  document.querySelector("#top-add").addEventListener("click", openRegister);
  document.querySelector("#rail-add").addEventListener("click", openRegister);
  document.querySelectorAll(".dialog-close").forEach(button => button.addEventListener("click", () => button.closest("dialog").close()));
  document.querySelectorAll("dialog").forEach(dialog => dialog.addEventListener("click", event => {
    const rect = dialog.getBoundingClientRect();
    if (event.clientX < rect.left || event.clientX > rect.right || event.clientY < rect.top || event.clientY > rect.bottom) dialog.close();
  }));
  document.querySelector("#register-form").addEventListener("submit", async event => {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    const submit = event.currentTarget.querySelector("[type=submit]");
    submit.disabled = true;
    submit.textContent = "Registering…";
    clearError("register-error");
    try {
      const result = await api("/api/v1/projects", { method: "POST", body: JSON.stringify({ root: form.get("root"), name: form.get("name") }) });
      state.selectedProject = result.project.id;
      registerDialog.close();
      event.currentTarget.reset();
      toast(`Registered ${result.project.name}. No files were adopted.`);
      await refresh({ quiet: true });
    } catch (error) {
      showError("register-error", error);
    } finally {
      submit.disabled = false;
      submit.textContent = "Register checkout";
    }
  });
  document.querySelector("#entry-form").addEventListener("submit", event => {
    event.preventDefault();
    requestPlan("adopt", new FormData(event.currentTarget).get("path"));
  });
  applyButton.addEventListener("click", applyPlan);

  refresh();
})();
