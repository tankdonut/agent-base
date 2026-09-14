// agentctl fleet console — a deliberately small vanilla-JS SPA over
// the serve API. Hash routing, fetch + Bearer from localStorage,
// EventSource for job/approval deltas. No framework, no build step.
(() => {
  const TOKEN_KEY = "agentctl-serve-token";
  let token = localStorage.getItem(TOKEN_KEY) || "";
  if (!token) {
    token = prompt("Serve API bearer token (see `fleet serve` output):");
    if (!token) { document.getElementById("view").textContent = "token required"; return; }
    localStorage.setItem(TOKEN_KEY, token);
  }

  async function api(path, opts = {}) {
    const res = await fetch(path, {
      ...opts,
      headers: {
        "Authorization": "Bearer " + token,
        ...(opts.body ? { "Content-Type": "application/json" } : {}),
      },
    });
    if (res.status === 401) {
      localStorage.removeItem(TOKEN_KEY);
      location.reload();
      throw new Error("unauthorized");
    }
    if (!res.ok) {
      const body = await res.json().catch(() => ({}));
      throw new Error(body.error || res.statusText);
    }
    return res.status === 204 ? null : res.json();
  }

  const view = () => document.getElementById("view");
  const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

  // ---- SSE: live badge + job log refresh ----
  let jobWatchers = {};
  function connectEvents() {
    const badge = document.getElementById("conn");
    const es = new EventSource("/api/v1/events?token=" + encodeURIComponent(token));
    es.onopen = () => { badge.textContent = "SSE: live"; badge.className = "badge ok"; };
    es.onerror = () => { badge.textContent = "SSE: down"; badge.className = "badge err"; };
    es.addEventListener("job", (e) => {
      const evt = JSON.parse(e.data);
      const w = jobWatchers[evt.job_id];
      if (w) w(evt.state);
    });
    es.addEventListener("approvals", () => {
      if (location.hash.startsWith("#/approvals")) route();
    });
  }

  // ---- views ----
  async function overview() {
    const roster = await api("/api/v1/roster");
    const status = await api("/api/v1/status").catch(() => ({ agents: [] }));
    const rows = roster.agents.map((a) => {
      const live = (status.agents || []).find((s) => s.agent === a.agent) || {};
      const gw = live.gateway || "unreachable";
      const cls = gw === "unreachable" || gw === "no token" ? "err" : "ok";
      return `<tr><td>${esc(a.agent)}</td><td>${esc(a.platform)}</td>` +
        `<td>${a.gateway_port}</td>` +
        `<td class="${cls}">${esc(gw)}${live.pending_approvals ? ` · <span class="warn">${live.pending_approvals} pending</span>` : ""}</td></tr>`;
    }).join("");
    const plane = roster.plane && roster.plane.enabled
      ? `<div class="card">plane <b>${esc(roster.plane.name)}</b> · litellm: ${esc(roster.plane.litellm)} · observability: ${roster.plane.observability ? "on" : "off"}</div>`
      : `<div class="card log">plane disabled</div>`;
    view().innerHTML = `<h2>Overview</h2>${plane}
      <table><tr><th>Agent</th><th>Platform</th><th>Gateway port</th><th>Gateway</th></tr>${rows}</table>`;
  }

  async function deploy() {
    const roster = await api("/api/v1/roster");
    const options = roster.agents.map((a) => `<option>${esc(a.agent)}</option>`).join("");
    view().innerHTML = `<h2>Deploy</h2>
      <div class="card">
        <label>Agent <select id="d-agent"><option value="">(fleet of one)</option>${options}</select></label>
        <label><input type="checkbox" id="d-all"> all agents</label>
        <label><input type="checkbox" id="d-force"> force recreate</label>
        <button id="d-go">Deploy</button>
        <span id="d-msg" class="log"></span>
      </div>
      <div class="card"><h3>Job output</h3><pre id="d-log" class="log">(idle)</pre></div>`;
    document.getElementById("d-go").onclick = async () => {
      const body = JSON.stringify({
        agent: document.getElementById("d-agent").value || undefined,
        all: document.getElementById("d-all").checked,
        force: document.getElementById("d-force").checked,
      });
      try {
        const res = await api("/api/v1/deploy", { method: "POST", body });
        document.getElementById("d-msg").textContent = "job " + res.job_id;
        watchJob(res.job_id, "d-log");
      } catch (e) { document.getElementById("d-msg").textContent = e.message; }
    };
  }

  function watchJob(id, preId) {
    const pre = document.getElementById(preId);
    jobWatchers[id] = async (state) => {
      try {
        const job = await api("/api/v1/jobs/" + id);
        pre.textContent = `[${job.state}] ` + (job.error || "") + "\n" + (job.log || []).join("");
        if (state === "done" || state === "failed") delete jobWatchers[id];
      } catch (_) {}
    };
    jobWatchers[id]("running");
  }

  async function jobs() {
    const data = await api("/api/v1/jobs");
    const rows = (data.jobs || []).slice().reverse().map((j) =>
      `<tr><td>${esc(j.id)}</td><td>${esc(j.kind)}</td>` +
      `<td class="${j.state === "failed" ? "err" : j.state === "done" ? "ok" : ""}">${esc(j.state)}</td>` +
      `<td>${esc((j.agents || []).join(", "))}</td>` +
      `<td><button class="ghost" onclick="window.__showJob('${esc(j.id)}')">log</button></td></tr>`).join("");
    view().innerHTML = `<h2>Jobs</h2>
      <table><tr><th>ID</th><th>Kind</th><th>State</th><th>Agents</th><th></th></tr>${rows}</table>
      <pre id="job-log" class="log">(select a job)</pre>`;
    window.__showJob = async (id) => {
      const job = await api("/api/v1/jobs/" + id);
      document.getElementById("job-log").textContent =
        `[${job.state}] ` + (job.error || "") + "\n" + (job.log || []).join("");
    };
  }

  async function approvals() {
    let data;
    try { data = await api("/api/v1/approvals"); }
    catch (e) {
      view().innerHTML = `<h2>Approvals</h2><div class="card err">${esc(e.message)}</div>`;
      return;
    }
    const rows = (data.approvals || []).map((a) => {
      const what = a.command || a.summary || "";
      return `<tr><td>${esc(a.id)}</td><td>${esc(a.kind || "")}</td><td>${esc(what)}</td>` +
        `<td><button onclick="window.__resolve('${esc(a.id)}','approve')">approve</button> ` +
        `<button class="danger" onclick="window.__resolve('${esc(a.id)}','deny')">deny</button></td></tr>`;
    }).join("");
    view().innerHTML = `<h2>Approvals — ${esc(data.agent || "")}</h2>` +
      ((data.approvals || []).length
        ? `<table><tr><th>ID</th><th>Kind</th><th>Request</th><th></th></tr>${rows}</table>`
        : `<div class="card ok">no pending approvals</div>`);
    window.__resolve = async (id, decision) => {
      try {
        await api("/api/v1/approvals/resolve", { method: "POST", body: JSON.stringify({ agent: data.agent, id, decision }) });
        route();
      } catch (e) { alert(e.message); }
    };
  }

  async function upgrades() {
    const roster = await api("/api/v1/roster");
    const target = document.getElementById("u-target") ? document.getElementById("u-target").value : "";
    const cards = await Promise.all(roster.agents.map(async (a) => {
      let body = `<div class="card"><h3>${esc(a.agent)}</h3><span class="log">pick a target tag</span></div>`;
      if (target) {
        try {
          const res = await api(`/api/v1/upgrades?agent=${encodeURIComponent(a.agent)}&target=${encodeURIComponent(target)}`);
          const p = res.preview;
          const crossings = (p.crossings || []).map((c) =>
            `<div class="${c.severity === "fail" ? "err" : "warn"}">↯ ${esc(c.release)} ${esc(c.id)}: ${esc(c.summary)} → ${esc(c.action)}</div>`).join("");
          body = `<div class="card"><h3>${esc(p.agent)} · ${esc(p.current_tag)} → ${esc(p.target)}</h3>` +
            (p.downgrade ? `<div class="err">downgrade — not modeled; rollback = restore backup</div>` :
             (p.crossings.length ? crossings : `<div class="ok">no era crossings</div>`)) +
            `<p><button onclick="window.__apply('${esc(p.agent)}','${esc(p.target)}')">Upgrade ${esc(p.agent)}</button></p></div>`;
        } catch (e) {
          body = `<div class="card"><h3>${esc(a.agent)}</h3><span class="err">${esc(e.message)}</span></div>`;
        }
      }
      return body;
    }));
    view().innerHTML = `<h2>Upgrades</h2>
      <div class="card"><label>Target tag <input id="u-target" placeholder="2026.09.14" value="${esc(target)}">
      <button onclick="route()">Preview</button></label>
      <span class="log">preview walks the era table; apply retags the Dockerfile and converges</span></div>${cards.join("")}`;
    window.__apply = async (agent, tag) => {
      if (!confirm(`Upgrade ${agent} to ${tag}?`)) return;
      try {
        const res = await api("/api/v1/upgrades/apply", { method: "POST", body: JSON.stringify({ agent, tag }) });
        location.hash = "#/jobs";
        setTimeout(() => watchJob(res.job_id, "job-log"), 300);
      } catch (e) { alert(e.message); }
    };
  }

  const routes = { "#/overview": overview, "#/deploy": deploy, "#/jobs": jobs,
                   "#/approvals": approvals, "#/upgrades": upgrades };
  function route() {
    const hash = location.hash || "#/overview";
    document.querySelectorAll("nav a").forEach((a) =>
      a.classList.toggle("active", a.getAttribute("href") === hash));
    const fn = routes[hash] || overview;
    fn().catch((e) => { view().innerHTML = `<div class="card err">${esc(e.message)}</div>`; });
  }
  window.addEventListener("hashchange", route);
  route();
  connectEvents();
})();
