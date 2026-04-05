const state = {
  currentView: "dashboard",
  authed: false,
};

const views = ["dashboard", "requests", "usage", "keys", "models", "settings"];

document.addEventListener("DOMContentLoaded", async () => {
  bindNav();
  bindLogin();
  bindActions();
  bindForms();
  await checkSession();
});

function bindNav() {
  document.querySelectorAll(".nav-link").forEach((button) => {
    button.addEventListener("click", () => setView(button.dataset.view));
  });
}

function bindLogin() {
  document.getElementById("login-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    const payload = {
      username: form.get("username"),
      password: form.get("password"),
    };
    try {
      await api("/api/admin/auth/login", { method: "POST", body: payload });
      document.getElementById("login-error").textContent = "";
      state.authed = true;
      document.getElementById("logout-btn").classList.remove("hidden");
      document.getElementById("login-card").classList.add("hidden");
      document.getElementById("app-content").classList.remove("hidden");
      await refreshCurrentView();
    } catch (error) {
      document.getElementById("login-error").textContent = error.message;
    }
  });
}

function bindActions() {
  document.getElementById("refresh-btn").addEventListener("click", refreshCurrentView);
  document.getElementById("logout-btn").addEventListener("click", async () => {
    await api("/api/admin/auth/logout", { method: "POST" });
    location.reload();
  });
}

function bindForms() {
  document.getElementById("key-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    const response = await api("/api/admin/keys", {
      method: "POST",
      body: {
        name: form.get("name"),
        content_logging: form.get("content_logging") === "on",
      },
    });
    document.getElementById("key-secret").classList.remove("hidden");
    document.getElementById("key-secret").textContent = `New key: ${response.key}`;
    event.currentTarget.reset();
    flash("Created a new client key. This is the only time the plaintext is shown.");
    await renderKeys();
  });

  document.getElementById("model-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    const payload = {
      alias: form.get("alias"),
      model_id: form.get("model_id"),
      region: form.get("region"),
    };
    const temp = form.get("temperature");
    const max = form.get("max_tokens");
    if (temp) payload.temperature = Number(temp);
    if (max) payload.max_tokens = Number(max);
    await api("/api/admin/models", { method: "POST", body: payload });
    event.currentTarget.reset();
    flash("Saved the model route.");
    await renderModels();
  });
}

async function checkSession() {
  try {
    await api("/api/admin/auth/me");
    state.authed = true;
    document.getElementById("logout-btn").classList.remove("hidden");
    document.getElementById("login-card").classList.add("hidden");
    document.getElementById("app-content").classList.remove("hidden");
    await refreshCurrentView();
  } catch {
    state.authed = false;
    document.getElementById("login-card").classList.remove("hidden");
    document.getElementById("app-content").classList.add("hidden");
  }
}

async function refreshCurrentView() {
  if (!state.authed) return;
  switch (state.currentView) {
    case "dashboard":
      await renderDashboard();
      break;
    case "requests":
      await renderRequests();
      break;
    case "usage":
      await renderUsage();
      break;
    case "keys":
      await renderKeys();
      break;
    case "models":
      await renderModels();
      break;
    case "settings":
      await renderSettings();
      break;
  }
}

function setView(name) {
  state.currentView = name;
  document.getElementById("view-title").textContent = titleCase(name);
  views.forEach((view) => {
    document.getElementById(`${view}-view`).classList.toggle("hidden", view !== name);
    document.querySelector(`.nav-link[data-view="${view}"]`).classList.toggle("active", view === name);
  });
  refreshCurrentView();
}

async function renderDashboard() {
  const dashboard = await api("/api/admin/dashboard");
  const usage = await api("/api/admin/usage");
  document.getElementById("hero-meta").textContent = `Started ${formatDateTime(dashboard.summary.started_at)}. Version ${dashboard.summary.version}.`;
  const metrics = dashboard.metrics;
  const cards = [
    ["Requests", metrics.total_requests],
    ["Successes", metrics.success_requests],
    ["Errors", metrics.error_requests],
    ["Input tokens", metrics.total_input_tokens],
    ["Output tokens", metrics.total_output_tokens],
    ["Estimated USD", formatCurrency(metrics.total_cost_usd)],
  ];
  document.getElementById("metrics-grid").innerHTML = cards
    .map(
      ([label, value]) => `
        <article class="metric-card">
          <span class="eyebrow">${label}</span>
          <strong>${value}</strong>
        </article>
      `,
    )
    .join("");
  const max = Math.max(1, ...usage.items.map((item) => item.total_tokens));
  document.getElementById("usage-chart").innerHTML = usage.items
    .slice(0, 8)
    .map(
      (item) => `
        <div class="usage-row">
          <div>
            <strong>${escapeHTML(item.model)}</strong>
            <div class="muted">${escapeHTML(item.bucket_date)} · ${escapeHTML(item.api_key_name || "unattributed")}</div>
          </div>
          <div class="usage-bar"><span style="width:${Math.max(6, (item.total_tokens / max) * 100)}%"></span></div>
          <div class="muted">${item.total_tokens.toLocaleString()} tokens</div>
        </div>
      `,
    )
    .join("");
}

async function renderRequests() {
  const response = await api("/api/admin/requests?limit=50");
  const rows = response.items
    .map(
      (item) => `
      <tr>
        <td>${formatDateTime(item.started_at)}</td>
        <td>${escapeHTML(item.api_key_name || item.api_key_id)}</td>
        <td>${escapeHTML(item.model)}</td>
        <td>${item.status_code}</td>
        <td>${item.total_tokens}</td>
        <td>${formatCurrency(item.estimated_cost_usd)}</td>
        <td>${item.latency_ms} ms</td>
        <td>${item.content_logged ? escapeHTML(shorten(item.response_text || item.request_json || "", 96)) : "<span class='muted'>metadata only</span>"}</td>
      </tr>`,
    )
    .join("");
  document.getElementById("requests-table").innerHTML = tableHTML(
    ["Time", "Key", "Model", "Status", "Tokens", "Cost", "Latency", "Log sample"],
    rows,
  );
}

async function renderUsage() {
  const response = await api("/api/admin/usage");
  const rows = response.items
    .map(
      (item) => `
      <tr>
        <td>${escapeHTML(item.bucket_date)}</td>
        <td>${escapeHTML(item.model)}</td>
        <td>${escapeHTML(item.api_key_name || "unattributed")}</td>
        <td>${item.requests}</td>
        <td>${item.input_tokens}</td>
        <td>${item.output_tokens}</td>
        <td>${item.total_tokens}</td>
        <td>${formatCurrency(item.estimated_cost_usd)}</td>
      </tr>`,
    )
    .join("");
  document.getElementById("usage-table").innerHTML = tableHTML(
    ["Date", "Model", "Key", "Requests", "Input", "Output", "Total", "Cost"],
    rows,
  );
}

async function renderKeys() {
  const response = await api("/api/admin/keys");
  const rows = response.items
    .map(
      (item) => `
      <tr>
        <td>${escapeHTML(item.name)}</td>
        <td>${escapeHTML(item.key_prefix)}</td>
        <td>${item.content_logging ? "full content" : "metadata only"}</td>
        <td>${item.enabled ? "active" : "revoked"}</td>
        <td>${formatDateTime(item.last_used_at)}</td>
        <td>${item.enabled ? `<button class="danger-button" data-revoke="${item.id}">Revoke</button>` : ""}</td>
      </tr>`,
    )
    .join("");
  document.getElementById("keys-table").innerHTML = tableHTML(
    ["Name", "Prefix", "Logging", "Status", "Last used", ""],
    rows,
  );
  document.querySelectorAll("[data-revoke]").forEach((button) => {
    button.addEventListener("click", async () => {
      await api(`/api/admin/keys/${button.dataset.revoke}/revoke`, { method: "POST" });
      flash("Revoked the client key.");
      await renderKeys();
    });
  });
}

async function renderModels() {
  const response = await api("/api/admin/models");
  const rows = response.items
    .map(
      (item) => `
      <tr>
        <td>${escapeHTML(item.alias)}</td>
        <td>${escapeHTML(item.bedrock_model_id)}</td>
        <td>${escapeHTML(item.region)}</td>
        <td>${item.default_temperature ?? "<span class='muted'>default</span>"}</td>
        <td>${item.default_max_tokens ?? "<span class='muted'>default</span>"}</td>
        <td>${item.enabled ? "enabled" : "disabled"}</td>
      </tr>`,
    )
    .join("");
  document.getElementById("models-table").innerHTML = tableHTML(
    ["Alias", "Bedrock model", "Region", "Temp", "Max tokens", "Status"],
    rows,
  );
}

async function renderSettings() {
  const settings = await api("/api/admin/settings");
  document.getElementById("settings-json").textContent = JSON.stringify(settings, null, 2);
}

async function api(url, options = {}) {
  const response = await fetch(url, {
    method: options.method || "GET",
    headers: {
      "Content-Type": "application/json",
    },
    credentials: "same-origin",
    body: options.body ? JSON.stringify(options.body) : undefined,
  });
  if (!response.ok) {
    let message = `Request failed: ${response.status}`;
    try {
      const data = await response.json();
      message = data.error?.message || message;
    } catch {}
    throw new Error(message);
  }
  return response.json();
}

function tableHTML(headers, rows) {
  return `
    <table>
      <thead>
        <tr>${headers.map((header) => `<th>${header}</th>`).join("")}</tr>
      </thead>
      <tbody>${rows || `<tr><td colspan="${headers.length}" class="muted">No data yet</td></tr>`}</tbody>
    </table>
  `;
}

function flash(message) {
  const node = document.getElementById("flash");
  node.textContent = message;
  node.classList.remove("hidden");
  clearTimeout(flash._timer);
  flash._timer = setTimeout(() => node.classList.add("hidden"), 2800);
}

function formatCurrency(value) {
  const number = Number(value || 0);
  return number === 0 ? "$0.00" : `$${number.toFixed(6)}`;
}

function formatDateTime(value) {
  if (!value) return "Never";
  return new Date(value).toLocaleString();
}

function shorten(value, max) {
  if (value.length <= max) return escapeHTML(value);
  return `${escapeHTML(value.slice(0, max))}…`;
}

function titleCase(value) {
  return value.charAt(0).toUpperCase() + value.slice(1);
}

function escapeHTML(value) {
  return String(value)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#039;");
}
