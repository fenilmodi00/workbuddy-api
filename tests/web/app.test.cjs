// Uses real front-end functions and synthetic responses; no browser/third-party dependencies and no production API calls.
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const {randomUUID} = require("node:crypto");

const projectRoot = path.resolve(__dirname, "../..");
const testDate = new Date().toLocaleDateString("sv-SE", {timeZone: "Asia/Shanghai"});
const testTrace = "webui-tests-" + randomUUID();
const logPath = path.join(projectRoot, "logs", "webui-frontend-tests-dev-" + testDate + ".jsonl");
function audit(event, fields = {}) {
  fs.mkdirSync(path.dirname(logPath), {recursive: true});
  fs.appendFileSync(logPath, JSON.stringify({traceId: testTrace, event, ...fields}) + "\n");
}
function response(status, body) {
  return {status, ok: status >= 200 && status < 300, json: async () => body};
}
function fixture(fetchImpl = async () => response(200, {}), cookies = new Map(), clock) {
  const elements = new Map(), session = new Map(), local = new Map(), alerts = [], timers = new Map();
  let timerID = 0;
  const cookieWrites = [];
  function element(id) {
    if (!elements.has(id)) {
      const node = {id, innerHTML: "", textContent: "", className: "", value: "", checked: false, disabled: false,
            style: {}, querySelector() { return this; }, querySelectorAll() { return []; },
            addEventListener() {}, focus() {}, removeAttribute() {}, setAttribute() {}};
      node.classList = {
        add(name) { node.className += " " + name; },
        remove(name) { node.className = node.className.split(/\s+/).filter(v => v !== name).join(" "); },
        toggle(name, force) {
          const present = node.className.split(/\s+/).includes(name);
          const enabled = force === undefined ? !present : force;
          if (enabled && !present) this.add(name);
          if (!enabled && present) this.remove(name);
        },
        contains(name) { return node.className.split(/\s+/).includes(name); },
      };
      elements.set(id, node);
    }
    return elements.get(id);
  }
  const storage = map => ({getItem: k => map.get(k) || null, setItem: (k, v) => map.set(k, v), removeItem: k => map.delete(k)});
  const context = {
    document: {readyState: "loading", addEventListener() {}, getElementById: element, querySelectorAll: () => []},
    sessionStorage: storage(session), localStorage: storage(local),
    window: {alert: message => alerts.push(message), confirm: () => true, addEventListener() {}, location: {protocol: "http:", reload() {}}},
    TextEncoder, fetch: fetchImpl, setInterval: () => 1,
    setTimeout: (fn) => { timers.set(++timerID, fn); return timerID; },
    clearTimeout: id => timers.delete(id),
  };
  if (typeof clock === "number") {
    context.Date = class extends Date {
      constructor(...args) { super(...(args.length ? args : [clock])); }
      static now() { return clock; }
    };
  }
  Object.defineProperty(context.document, "cookie", {
    configurable: true,
    get() { return [...cookies].map(([name, value]) => name + "=" + value).join("; "); },
    set(value) {
      cookieWrites.push(value);
      const pair = value.split(";")[0];
      const at = pair.indexOf("=");
      cookies.set(pair.slice(0, at), pair.slice(at + 1));
    },
  });
  let source = fs.readFileSync(path.join(projectRoot, "web", "app.js"), "utf8");
  source = source.replace(/\}\)\(\);\s*$/, `globalThis.review = {
    state, api, showAlert, hideAlert, togglePolicy, renderAccounts, renderModels, renderStatus, refreshActive,
    renderPoolHealth, renderAccountAlerts, bindAccountAlertTooltip, loadAccountAlerts, lockDashboard,
    saveKey, saveAPISettings, initializeAdmin, discoverSetup,
    showConfigWarning, loadConfigEditor, saveConfigEditor, exitConfigEdit, setTab, lockDashboard,
    initListFilters, changeListFilter, reportRestoredListFilters,
    rateCell, rateKey, rateDetailHTML, ensureRateDetails, toggleRateDetail, restoreRateDetails
    , renderStats, loadStats, changeStatsFilter, loadLogs, statsLineChart, statsComboChart, statsDuration, statsNumber, statsCredit
    , openAddAccount, startAddAccount, pollAddAccount, closeAddAccount, accountRowActions
    , initStatsFilter, writeStatsFilterCookie, readStatsFilterCookie, bindStatsChartHover
  };})();`);
  vm.createContext(context);
  vm.runInContext(source, context);
  return {review: context.review, element, session, local, alerts, window: context.window,
    document: context.document, cookies, cookieWrites, timers};
}
function cells(html) {
  return [...html.matchAll(/<td(?: [^>]*)?>(.*?)<\/td>/g)].map(m => m[1]);
}

test("Success alerts survive polling, close after five seconds, and a stale timer cannot close a new error", async () => {
  const f = fixture();
  f.review.state.apiKey = "synthetic-notification-key";
  f.review.showAlert("Synthetic toggle success", true);
  const oldTimer = [...f.timers.values()][0];
  await f.review.api("/admin/api/status");
  assert.equal(f.element("alert").className, "alert ok");
  f.review.showAlert("Synthetic error", false, "/admin/api/account-toggle");
  oldTimer();
  assert.equal(f.element("alert").className, "alert", "A late success timer must not swallow a new error");
  f.review.showAlert("Success again", true);
  [...f.timers.values()][0]();
  assert.equal(f.element("alert").className, "alert hidden");
  audit("Success alert anti-flash passed", {poll_preserves_success: true, old_timer_guard: true});
});

test("Unrelated requests cannot clear an error; only recovery on the same route closes it; quiet failures are still audited", async () => {
  let quiet = false;
  const calls = [];
  const f = fixture(async (path, options) => {
    calls.push({path, options});
    if (quiet && path === "/admin/api/stats") throw new Error("synthetic network error");
    return response(200, {});
  });
  f.review.state.apiKey = "synthetic-error-key";
  f.review.showAlert("Toggle save failed", false, "/admin/api/account-toggle");
  await f.review.api("/admin/api/status");
  assert.equal(f.element("alert").textContent, "Toggle save failed");
  assert.equal(f.element("alert").className, "alert");
  await f.review.api("/admin/api/account-toggle");
  assert.equal(f.element("alert").className, "alert hidden");
  quiet = true;
  await assert.rejects(f.review.api("/admin/api/stats", {quiet: true, trace: "synthetic-quiet-trace"}));
  assert.equal(f.element("alert").className, "alert hidden");
  assert(calls.some(c => c.path === "/admin/api/client-events" &&
    c.options.headers["X-Trace-ID"] === "synthetic-quiet-trace" &&
    JSON.parse(c.options.body).event === "request_failed"));
  audit("Error attribution and quiet requests passed", {unrelated_poll_cannot_hide_error: true, failure_audited: true});
});

test("Repeated polling with identical account and model data does not rewrite tbody; changes and clears still update", () => {
  const f = fixture();
  for (const [id, render, payload, changed] of [
    ["accountsTable", f.review.renderAccounts,
      [{path: "synthetic.json", state: "active", requests: 1}],
      [{path: "synthetic.json", state: "active", nickname: "Updated"}]],
    ["modelsTable", f.review.renderModels,
      {models: [{id: "synthetic-model", requests: 1}]},
      {models: [{id: "synthetic-model", requests: 2}]}],
  ]) {
    const tb = f.element(id);
    let html = "", writes = 0;
    Object.defineProperty(tb, "innerHTML", {configurable: true,
      get: () => html, set: value => { html = value; writes++; }});
    render(payload);
    render(payload);
    assert.equal(writes, 1, id + ": identical refresh replaced markup");
    render(changed);
    assert.equal(writes, 2, id + ": changed data was not rendered");
    render(id === "accountsTable" ? [] : {models: []});
    assert.equal(writes, 3);
    assert.match(html, /No .* yet/);
  }
  f.review.lockDashboard();
  assert.equal(f.element("accountsTable")._tableSourceHTML, "");
  assert.equal(f.element("modelsTable")._tableSourceHTML, "");
  audit("Repeated polling skips table rewrite passed", {accounts: true, models: true, real_changes_preserved: true});
});

test("Runtime status shows only health; no accounts and stale snapshots cannot show 100%", () => {
  const f = fixture();
  f.review.renderPoolHealth({health: {percent: 100, state: "healthy"}});
  assert.equal(f.element("poolHealth").textContent, "100%");
  assert.equal(f.element("poolHealthBar").style.width, "100%");
  f.review.renderPoolHealth({health: {percent: 66.6, state: "degraded"}});
  assert.equal(f.element("poolHealth").textContent, "66.6%");
  assert.match(f.element("poolHealthStatus").textContent, /partially limited/);
  f.review.renderPoolHealth({health: {percent: null, state: "empty"}});
  assert.equal(f.element("poolHealth").textContent, "—");
  assert.match(f.element("poolHealthStatus").textContent, /No accounts loaded/);
  f.review.renderPoolHealth({stale: true, health: {percent: 100, state: "healthy"}});
  assert.equal(f.element("poolHealth").textContent, "—");
  assert.equal(f.element("poolHealthBar").style.width, "0%");
  const html = fs.readFileSync(path.join(projectRoot, "web/index.html"), "utf8");
  assert(!html.includes('id="summaryCards"'));
  assert(!html.includes('id="siteSummary"'));
  assert(html.includes('id="accountsTable"'), "Detailed account pool status must not be removed");
  audit("Account pool health passed", {unknown_not_100: true, count_cards_removed: true});
});
test("Alert details survive the first click after focus, toggle on repeat clicks, and unchanged polling never clears them", () => {
  const f = fixture();
  const handlers = {};
  const list = f.element("accountAlerts"), tip = f.element("accountAlertTooltip");
  list.addEventListener = (name, handler) => { handlers[name] = handler; };
  const row = {
    closest: () => row,
    getAttribute: () => "Account disabled\nFull reason",
    getBoundingClientRect: () => ({left: 30, bottom: 40}),
  };
  f.window.innerWidth = 1920;
  f.window.innerHeight = 1080;
  f.review.bindAccountAlertTooltip();
  handlers.focusin({target: row});
  handlers.click({target: row});
  assert.equal(tip.hidden, false, "Focus showing first must not cause the first click to close immediately");
  assert.match(tip.textContent, /Full reason/);
  handlers.pointerleave({pointerType: "touch"});
  assert.equal(tip.hidden, false, "A touch pointerleave must not close already-open details");
  handlers.click({target: row});
  assert.equal(tip.hidden, true);
  handlers.pointermove({target: row, pointerType: "mouse", clientX: 30, clientY: 40});
  handlers.click({target: row});
  assert.equal(tip.hidden, false, "Hover before click must not close it either");
  const data = {alerts: [{account: "synthetic.json", severity: "notice",
    label: "Account disabled", message: 'Full reason "quotation" <tag>'}]};
  f.review.renderAccountAlerts(data);
  tip.hidden = false;
  f.review.renderAccountAlerts(data);
  assert.equal(tip.hidden, false, "Polling with an identical alert must not close the tooltip details");
  f.review.renderAccountAlerts({alerts: []});
  assert.equal(tip.hidden, true, "A recovered alert must not leave stale details behind");
  audit("Alert detail interaction regression passed", {first_tap_visible: true, polling_preserves_details: true});
});
test("Runtime status (health and alerts) comes first in the overview, with usage and performance after it", () => {
  const html = fs.readFileSync(path.join(projectRoot, "web/index.html"), "utf8");
  const start = html.indexOf('id="panel-overview"');
  const end = html.indexOf('id="panel-accounts"');
  assert(start > -1 && end > start, "The overview panel must exist");
  const overview = html.slice(start, end);
  const heading = overview.indexOf("overview-subheading");
  const health = overview.indexOf("health-layout");
  const usage = overview.indexOf("usage-section");
  const info = overview.indexOf("gatewayInfo");
  assert(heading > -1 && health > -1 && usage > -1 && info > -1, "The overview must contain runtime status, usage, and runtime info");
  assert(heading < health && health < usage, "Runtime status must come before usage and performance");
  assert(usage < info, "Runtime info should still come last");
  audit("Overview section order passed", {health_first: true, usage_after_health: true});
});


test("Account log alerts severity levels, sensitive text escaping, recovered-record removal, and logout cleanup", () => {
  const f = fixture();
  const alerts = [
    {account: "paused.json", state: "paused", severity: "notice", label: "Account disabled", message: "Manually disabled", at: 1791980002, logFile: "gateway.log"},
    {account: "dead<script>.json", state: "disabled", severity: "urgent", label: "Account invalid", message: '<img src=x onerror="alert(1)">', at: 1791980001, logFile: "gateway.log"},
    {account: "expired.json", state: "expired", severity: "important", label: "Account expired", message: "Check renewal", at: 1791980000, logFile: "gateway-2026-10-14.log"}
  ];
  f.review.state.lastStatus = {accounts: [
    {path: "expired.json", state: "active"},
    {path: "dead<script>.json", state: "disabled"},
    {path: "paused.json", state: "active", schedulingEnabled: false}
  ]};
  f.review.renderAccountAlerts({alerts, source: "gateway regular log"});
  const html = f.element("accountAlerts").innerHTML;
  assert(!html.includes("expired.json"), "Expired alerts must not remain visible after recovery");
  assert(html.includes("alert-urgent") && html.includes("Urgent"));
  assert(html.includes("alert-notice") && html.includes("Notice"));
  assert(html.includes("&lt;script&gt;") && !html.includes("<img"));
  assert(html.indexOf("paused.json") < html.indexOf("dead&lt;script&gt;.json"), "The newer notice alert must come before the older urgent alert; the front end must not re-sort by severity");
  assert.equal(f.element("accountAlertsInfo").textContent, "", "The source note text has been removed");
  f.review.renderAccountAlerts({alerts: [], missingLogCount: 1, scanTruncated: true});
  assert.match(f.element("accountAlerts").innerHTML, /no matching logs were found/);
  assert.match(f.element("accountAlertsInfo").textContent, /Read limit/);
  f.review.lockDashboard();
  assert.equal(f.review.state.lastAccountAlerts, null);
  assert(!f.element("accountAlerts").innerHTML.includes("dead"));
  audit("Account alert display passed", {recovered_removed: true, escaped: true, logout_cleared: true});
});

test("Alert rows keep only a compact title; full content goes into tooltip data and the alert-details badge is used", () => {
  const f = fixture();
  f.review.state.lastStatus = {accounts: [{path: "paused.json", state: "active", schedulingEnabled: false}]};
  f.review.renderAccountAlerts({alerts: [{account: "paused.json", state: "paused", severity: "notice",
    label: "Account disabled", message: "This account was disabled by global scheduling rules; this is not an auth failure and can be restored in the account pool.", at: 1791980005}]});
  const html = f.element("accountAlerts").innerHTML;
  assert.match(html, /account-alert alert-notice/);
  assert.match(html, /data-alert-detail="[^"]*disabled by global scheduling rules/);
  assert(!/<p>This account was disabled by global scheduling rules/.test(html), "Alert body text must not occupy permanent height");
  const page = fs.readFileSync(path.join(projectRoot, "web/index.html"), "utf8");
  assert(page.includes('<span class="badge">Alert details</span>'));
  assert(!page.includes("Read-only logs"));
  audit("Compact alert tooltip passed", {compact_row: true, full_detail_in_data_attribute: true, badge_updated: true});
});

test("Runtime logs display with the newest line at the top", async () => {
  const f = fixture(async url => {
    assert(url.startsWith("/admin/api/logs?lines="));
    return response(200, {lines: ["oldest", "middle", "newest"], files: [], file: "gateway.log"});
  });
  f.review.state.apiKey = "synthetic-admin";
  f.element("logLines").value = "200";
  await f.review.loadLogs(true);
  assert.equal(f.element("logBox").textContent, "newest\nmiddle\noldest");
  assert.equal(f.element("logBox").scrollTop, 0);
  audit("Runtime logs reverse order passed", {newest_first: true});
});

test("The page removed the stats persistence badge and the summary-card quarter-circle decoration; tabs adapt to screen size", () => {
  const page = fs.readFileSync(path.join(projectRoot, "web/index.html"), "utf8");
  const css = fs.readFileSync(path.join(projectRoot, "web/style.css"), "utf8");
  assert(!page.includes('id="statsStorage"'));
  assert(!/\.usage-metric::after\s*\{/.test(css), "The summary-card circular decoration must be deleted");
  assert(/\.tabs\s*\{[\s\S]*width:\s*min\(83\.3333vw,\s*3200px\)/.test(css));
  assert(/\.tabs\s*\{[\s\S]*grid-template-columns:\s*repeat\(7,\s*minmax\(0,\s*1fr\)\)/.test(css));
  assert(/\.health-layout\s*\{[^}]*height:\s*196px/.test(css));
  assert(/\.account-alert\s*\{[^}]*padding:\s*3px 10px/.test(css));
  audit("Layout simplification and responsiveness passed", {stats_badge_removed: true, quarter_circle_removed: true, tabs_5_of_6_capped_4k: true});
});

test("A fresh status snapshot immediately removes old alerts without waiting for another log scan", () => {
  const f = fixture();
  const payload = {alerts: [
    {account: "cooling.json", state: "cooldown", severity: "warning", label: "Account cooling down",
      message: "Waiting to recover", at: 1791980005, logFile: "gateway.log"},
    {account: "paused.json", state: "paid_exhausted", severity: "important", label: "Credits exhausted",
      message: "Waiting for quota", at: 1791980004, logFile: "gateway.log"},
    {account: "paused.json", state: "paused", severity: "notice", label: "Account disabled",
      message: "Manually disabled", at: 1791980003, logFile: "gateway.log"}
  ]};
  f.review.state.lastStatus = {accounts: [
    {path: "cooling.json", state: "cooldown"},
    {path: "paused.json", state: "paid_exhausted", schedulingEnabled: false}
  ]};
  f.review.renderAccountAlerts(payload);
  assert.match(f.element("accountAlerts").innerHTML, /cooling down/);
  assert.match(f.element("accountAlerts").innerHTML, /Credits exhausted/);
  // renderStatus re-filters the existing payload on the normal status poll.
  f.review.renderStatus({accounts: [
    {path: "cooling.json", state: "active"},
    {path: "paused.json", state: "active", schedulingEnabled: false}
  ]});
  const html = f.element("accountAlerts").innerHTML;
  assert(!html.includes("cooling down") && !html.includes("Credits exhausted"));
  assert(html.includes("Account disabled"), "Quota recovery must not automatically undo a manual disable");
  audit("Fresh snapshot removes recovered alerts passed", {log_rescan_required: false, manual_pause_preserved: true});
});

test("Alert log load failures keep the previous result and never fake a no-anomaly state", async () => {
  const f = fixture(async route => route === "/admin/api/client-events" ? response(204, {}) : response(503, {error: "Logs unreadable"}));
  f.review.state.apiKey = "synthetic-admin";
  f.element("accountAlerts").innerHTML = "Existing alerts";
  await f.review.loadAccountAlerts();
  assert.equal(f.element("accountAlerts").innerHTML, "Existing alerts");
  assert.match(f.element("accountAlertsInfo").textContent, /does not mean the account pool has no anomalies/);
  assert.equal(f.review.state.accountAlertsPending, false);
  audit("Alert load failure keeps result passed");
});

test("Known zeros display 0; unknown quota still shows a dash", () => {
  const f = fixture();
  f.review.renderAccounts([{path: "fixture.json", state: "active", quotaKnown: true}]);
  let row = cells(f.element("accountsTable").innerHTML);
  for (const i of [7, 8, 9, 12, 14]) assert.equal(row[i], "0");
  assert.match(row[11], /Pending|Missed/, "The checkin column must no longer show the free model count");
  assert.equal(row.length, 16, "The account table should have 16 columns (the token expiry column was removed)");
  const accountsPage = fs.readFileSync(path.join(projectRoot, "web/index.html"), "utf8");
  assert(!accountsPage.includes("Token expiry"), "The table header must not keep the removed column");
  f.review.renderAccounts([{path: "fixture.json", state: "active", quotaKnown: false}]);
  row = cells(f.element("accountsTable").innerHTML);
  for (const i of [7, 8, 9]) assert.equal(row[i], "-");
  f.review.renderModels({models: [{id: "fixture-model"}]});
  assert.equal(cells(f.element("modelsTable").innerHTML)[4], "0");
  audit("Zero value display passed", {unknown_quota_preserved: true});
});

test("Enterprise unlimited shows Unlimited, used keeps decimals; unknown and finite quota are not conflated", () => {
  const f = fixture();
  f.review.renderAccounts([{path: "enterprise.json", state: "active", quotaKnown: true,
    quotaUnlimited: true, quotaTotal: -1, quotaUsed: 12.5, quotaRemaining: -1, planLabel: "Enterprise"}]);
  let row = cells(f.element("accountsTable").innerHTML);
  assert.equal(row[7], "Unlimited");
  assert.equal(row[8], "12.5");
  assert.equal(row[9], "Unlimited");
  assert.equal(row[10], "Enterprise");
  f.review.renderAccounts([{path: "enterprise.json", state: "active", quotaKnown: false, quotaUnlimited: true}]);
  row = cells(f.element("accountsTable").innerHTML);
  for (const i of [7, 8, 9]) assert.equal(row[i], "-");
  audit("Enterprise quota display passed", {unlimited_distinct_from_unknown: true, fractional_used_preserved: true});
});

test("The account pool checkin column shows Beijing-time same-day status, the free model column was removed, and error messages are escaped", () => {
  const clock = Date.parse("2026-10-25T08:20:00+08:00");
  const f = fixture(undefined, new Map(), clock);
  const account = {path: "synthetic.json", state: "active", freeModels: 987654};
  for (const [status, text] of [["pending", "Pending"], ["running", "Pending"], ["success", "Checked in"],
    ["failed", "Failed"], ["missed", "Missed"], ["unknown", "Unknown"]]) {
    f.review.renderAccounts([{...account, checkin: {date: "2026-10-25", status,
      message: '<img src=x onerror="bad">', nextAt: Date.parse("2026-10-25T09:45:00+08:00") / 1000}}]);
    const row = cells(f.element("accountsTable").innerHTML);
    assert.match(row[11], new RegExp(text));
    assert(!f.element("accountsTable").innerHTML.includes("987654"));
    assert(row[11].includes("&lt;img") && !row[11].includes("<img"));
    assert(row[11].includes("Beijing time 2026-10-25"));
  }
  f.review.renderAccounts([{...account, edition: "intl", checkin: {date: "2026-10-25", status: "pending"}}]);
  assert.match(cells(f.element("accountsTable").innerHTML)[11], />—</);
  f.review.renderAccounts([{...account, checkin: {date: "2026-10-25", status: "unsupported"}}]);
  assert.match(cells(f.element("accountsTable").innerHTML)[11], />—</);
  const html = fs.readFileSync(path.join(projectRoot, "web/index.html"), "utf8");
  assert(html.includes("<th>Checkin</th>") && !html.includes("<th>Free models</th>"));
  audit("Account pool checkin column passed", {beijing_day: true, statuses: 6, private_errors_escaped: true, free_model_column_removed: true});
});

test("After midnight Beijing time yesterday\u2019s Checked-in is not reused; a missed window shows Missed, not Checked-in or Failed", () => {
  for (const [clock, text] of [["2026-10-25T00:01:00+08:00", "Pending"], ["2026-10-25T09:31:00+08:00", "Missed"]]) {
    const f = fixture(undefined, new Map(), Date.parse(clock));
    f.review.renderAccounts([{path: "synthetic.json", state: "active",
      checkin: {date: "2026-10-24", status: "success", nextAt: 1900000000}}]);
    const cell = cells(f.element("accountsTable").innerHTML)[11];
    assert.match(cell, new RegExp(text));
    assert(!cell.includes("Checked in") && !cell.includes(" · Scheduled "));
  }
  audit("Checkin Beijing-time day rollover passed", {old_success_cleared: true, missed_window_not_a_failure: true});
});

test("A 503 snapshot keeps previous content and never fabricates zero accounts", async () => {
  const f = fixture(async route => route === "/admin/api/client-events" ? response(204, {}) : response(503, {error: "Status snapshot temporarily unavailable"}));
  f.review.state.apiKey = "synthetic-admin-key";
  f.review.renderStatus({gateway: {}, accounts: [{path: "existing.json", state: "active"}]});
  const before = f.element("accountsTable").innerHTML;
  await assert.rejects(f.review.api("/admin/api/status"));
  assert.equal(f.element("accountsTable").innerHTML, before);
  assert.match(f.element("alert").textContent, /503.*previous data preserved/);
  audit("Snapshot failure keeps old data passed");
});

test("Polling never overlaps and front/back audits reuse the same TraceID", async () => {
  let release;
  const calls = [];
  const f = fixture((route, options) => {
    calls.push({route, options});
    if (route === "/admin/api/client-events") return Promise.resolve(response(204, {}));
    return new Promise(resolve => { release = resolve; });
  });
  f.review.state.apiKey = "synthetic-admin-key";
  const first = f.review.refreshActive();
  await f.review.refreshActive();
  assert.equal(calls.filter(c => c.route === "/admin/api/status").length, 1);
  release(response(200, {gateway: {}, accounts: [], summary: {total: 0}}));
  await first;
  assert.equal(f.review.state.refreshPending, false);
  const event = calls.find(c => c.route === "/admin/api/client-events");
  assert.equal(event.options.headers["X-Trace-ID"], calls[0].options.headers["X-Trace-ID"]);
  assert(!event.options.body.includes("synthetic-admin-key"));
  audit("Polling and audit passed", {overlapping_status_requests: 0});
});

test("A wrong admin Key is not saved to session storage", async () => {
  const f = fixture(async () => response(401, {error: "Admin auth failed"}));
  f.element("apiKey").value = "wrong-synthetic-admin";
  await f.review.saveKey();
  assert.equal(f.session.size, 0);
  assert.equal(f.review.state.apiKey, "");
  audit("Wrong login not persisted passed");
});

test("First-time initialization alerts only once and logs in with the returned admin Key", async () => {
  const key = "a".repeat(32);
  let setups = 0;
  const f = fixture(async (route, options) => {
    if (route === "/admin/api/setup") {
      assert.equal(options.method, "POST");
      assert.equal(options.headers["X-Workbuddy-Admin"], "1");
      setups++;
      return response(201, {adminKey: key});
    }
    if (route === "/admin/api/settings") return response(200, {apiKeyEnabled: false, apiKeyConfigured: false, apiPort: 8317, webPort: 8316});
    if (route === "/admin/api/status") return response(200, {gateway: {}, accounts: []});
    return response(204, {});
  });
  await f.review.initializeAdmin();
  assert.equal(setups, 1);
  assert.equal(f.alerts.length, 1);
  assert(f.alerts[0].includes(key));
  assert.equal(f.session.get("wb_gateway_admin_key"), key);
  assert.equal(f.local.size, 0);
  audit("Initialization alert and login passed", {key_length: key.length, saved_to_localStorage: false});
});

test("Enabling is blocked without an API Key; turning the switch off can be saved", async () => {
  const calls = [];
  const f = fixture(async (route, options) => {
    calls.push({route, options});
    if (route === "/admin/api/settings") return response(200, {apiKeyEnabled: false, apiKeyConfigured: false});
    return response(204, {});
  });
  f.review.state.apiKey = "synthetic-admin-key";
  f.review.state.settings = {apiKeyConfigured: false};
  f.element("apiKeyEnabled").checked = true;
  f.review.saveAPISettings();
  assert.equal(calls.filter(c => c.route === "/admin/api/settings").length, 0);
  assert.match(f.element("alert").textContent, /Enter a Key/);
  f.element("apiKeyEnabled").checked = false;
  f.review.saveAPISettings();
  await new Promise(resolve => setImmediate(resolve));
  const saved = calls.find(c => c.route === "/admin/api/settings");
  assert.equal(JSON.parse(saved.options.body).apiKeyEnabled, false);
  audit("API auth switch form passed");
});

const configContent = JSON.stringify({gateway: {adminKey: "synthetic-admin-key", apiKey: "synthetic-private-api-key"}, systemPrompt: {force: "synthetic-private-prompt"}}, null, 2);
function configFixture(fetchImpl) {
  const calls = [];
  const f = fixture(async (route, options) => {
    calls.push({route, options});
    if (route === "/admin/api/client-events") return response(204, {});
    if (fetchImpl) {
      const result = await fetchImpl(route, options);
      if (result) return result;
    }
    if (route.startsWith("/admin/api/config-edit?")) return response(200, {content: configContent, revision: "original-revision"});
    if (route === "/admin/api/config") return response(200, {exists: true, content: "masked preview"});
    if (route === "/admin/api/settings") return response(200, {apiKeyConfigured: true, apiPort: 8317, webPort: 8316});
    return response(200, {saved: true, adminKeyChanged: false, settings: {}});
  });
  f.review.state.apiKey = "synthetic-admin-key";
  f.review.state.activeTab = "config";
  return {...f, calls};
}
async function openConfigEditor(f) {
  f.review.showConfigWarning();
  await f.review.loadConfigEditor();
}

test("Config editing warns first and only fetches the raw content after confirmation; the audit contains no body", async () => {
  const f = configFixture();
  await f.review.loadConfigEditor();
  assert.equal(f.calls.length, 0, "Before the warning state, raw content must not be loaded");
  f.review.showConfigWarning();
  assert.equal(f.review.state.configMode, "warning");
  assert.equal(f.calls.filter(c => c.route.startsWith("/admin/api/config-edit")).length, 0);
  await f.review.loadConfigEditor();
  assert.equal(f.review.state.configMode, "edit");
  assert.equal(f.element("configEditor").value, configContent);
  assert.equal(f.local.size, 0);
  assert.equal(f.session.size, 0);
  const request = f.calls.find(c => c.route.includes("?acknowledged=1"));
  const acknowledged = f.calls.find(c => c.route === "/admin/api/client-events" && JSON.parse(c.options.body).event === "config_edit_acknowledged");
  assert.equal(request.options.headers["X-Trace-ID"], acknowledged.options.headers["X-Trace-ID"]);
  for (const event of f.calls.filter(c => c.route === "/admin/api/client-events")) {
    assert(!event.options.body.includes("synthetic-private"));
  }
  audit("Edit warning and raw load passed", {persistent_config: false, trace_shared: true});
});

test("The editor rejects invalid JSON without submitting and keeps the input", async () => {
  const f = configFixture();
  await openConfigEditor(f);
  for (const input of ["{", "[]", "null", JSON.stringify({value: "x".repeat(1100000)})]) {
    f.element("configEditor").value = input;
    await f.review.saveConfigEditor();
    assert.equal(f.calls.filter(c => c.route === "/admin/api/config-edit" && c.options.method === "POST").length, 0);
    assert.equal(f.element("configEditor").value, input);
    assert.match(f.element("configEditorStatus").textContent, /Config not saved/);
  }
  audit("Editor local validation passed");
});

test("Version conflicts and server failures keep edits; never silently refresh over them", async () => {
  for (const status of [409, 500]) {
    const f = configFixture(async (route, options) => route === "/admin/api/config-edit" && options.method === "POST" ? response(status, {error: "Save not completed"}) : null);
    await openConfigEditor(f);
    const edited = configContent.replace("synthetic-private-prompt", "edited-private-prompt");
    f.element("configEditor").value = edited;
    await f.review.saveConfigEditor();
    assert.equal(f.element("configEditor").value, edited);
    assert.equal(f.review.state.configRevision, "original-revision");
    assert.equal(f.review.state.configPending, false);
    assert.equal(f.element("cancelConfigEdit").disabled, false);
    assert.match(f.element("configEditorStatus").textContent, status === 409 ? /Version conflict/ : /Save not completed/);
    assert.equal(f.calls.filter(c => c.route.includes("?acknowledged=1")).length, 1);
  }
  audit("Conflict and server failure keep input passed");
});

test("Late responses after cancellation are ignored and the Key is never re-exposed", async () => {
  let release;
  const f = configFixture((route) => route.includes("?acknowledged=1") ? new Promise(resolve => { release = resolve; }) : null);
  f.review.showConfigWarning();
  const loading = f.review.loadConfigEditor();
  assert.equal(f.review.exitConfigEdit(), true);
  release(response(200, {content: configContent, revision: "late-revision"}));
  await loading;
  assert.equal(f.element("configEditor").value, "");
  assert.equal(f.review.state.configOriginal, "");
  assert.equal(f.review.state.configRevision, "");
  assert.equal(f.review.state.configMode, "preview");
  audit("Late response after cancel isolated passed");
});

test("Leaving the config panel confirms discarding edits first and clears all raw edit content", async () => {
  const f = configFixture();
  await openConfigEditor(f);
  f.element("configEditor").value += " ";
  f.window.confirm = () => false;
  f.review.setTab("settings");
  assert.equal(f.review.state.activeTab, "config");
  assert(f.element("configEditor").value.includes("synthetic-private-api-key"));
  f.window.confirm = () => true;
  f.review.setTab("settings");
  assert.equal(f.review.state.activeTab, "settings");
  assert.equal(f.review.state.configOriginal, "");
  assert.equal(f.element("configEditor").value, "");
  audit("Leaving page clears raw edit content passed");
});

test("A successful save returns to the masked preview and the port hot-switch failure notice is preserved", async () => {
  const f = configFixture(async (route, options) => route === "/admin/api/config-edit" && options.method === "POST" ?
    response(200, {saved: true, adminKeyChanged: false, settings: {restartRequired: true}}) : null);
  await openConfigEditor(f);
  await f.review.saveConfigEditor();
  assert.equal(f.element("configEditor").value, "");
  assert.equal(f.review.state.configOriginal, "");
  assert.equal(f.review.state.configMode, "preview");
  assert.match(f.element("configResult").textContent, /listening port has not hot-switched/);
  const post = f.calls.find(c => c.route === "/admin/api/config-edit" && c.options.method === "POST");
  const body = JSON.parse(post.options.body);
  assert.equal(body.content, configContent);
  assert.equal(body.revision, "original-revision");
  assert.equal(body.acknowledged, true);
  audit("Save and port hot-switch notice passed");
});

test("An admin Key change clears raw edit content and the old session and requires re-login", async () => {
  const f = configFixture(async (route, options) => route === "/admin/api/config-edit" && options.method === "POST" ?
    response(200, {saved: true, adminKeyChanged: true, settings: {}}) : null);
  f.session.set("wb_gateway_admin_key", "synthetic-admin-key");
  await openConfigEditor(f);
  f.element("configEditor").value = configContent.replace("synthetic-admin-key", "new-private-admin-key");
  await f.review.saveConfigEditor();
  assert.equal(f.review.state.apiKey, "");
  assert.equal(f.session.size, 0);
  assert.equal(f.element("configEditor").value, "");
  assert.equal(f.review.state.configOriginal, "");
  assert.equal(f.element("apiKey").value, "");
  assert.match(f.element("alert").textContent, /log in again with the new Key/);
  audit("Admin Key change clears session passed");
});

test("Clearing the admin Key requires extra confirmation; canceling never starts a save", async () => {
  const f = configFixture();
  await openConfigEditor(f);
  f.element("configEditor").value = "{}";
  f.window.confirm = () => false;
  await f.review.saveConfigEditor();
  assert.equal(f.calls.filter(c => c.route === "/admin/api/config-edit" && c.options.method === "POST").length, 0);
  assert.equal(f.element("configEditor").value, "{}");
  audit("Admin Key clearing extra confirmation passed");
});

test("The account pool filter keeps only usable schedulable accounts; unchecking restores all", () => {
  const f = fixture();
  f.review.initListFilters();
  const accounts = [
    {path: "active.json", state: "active", schedulingEnabled: true},
    {path: "paused.json", state: "active", schedulingEnabled: false},
    {path: "cooldown.json", state: "cooldown", schedulingEnabled: true},
    {path: "expired.json", state: "expired"},
    {path: "disabled.json", state: "disabled"},
    {path: "exhausted.json", state: "paid_exhausted"},
  ];
  f.review.renderStatus({gateway: {}, accounts});
  assert(f.element("accountsTable").innerHTML.includes("paused.json"));
  f.element("onlyUsableAccounts").checked = true;
  f.review.changeListFilter("accounts");
  let html = f.element("accountsTable").innerHTML;
  assert(html.includes("active.json"));
  for (const name of ["paused", "cooldown", "expired", "disabled", "exhausted"]) assert(!html.includes(name + ".json"));
  assert.equal(f.review.state.lastStatus.accounts.length, 6);
  f.element("onlyUsableAccounts").checked = false;
  f.review.changeListFilter("accounts");
  assert(f.element("accountsTable").innerHTML.includes("paused.json"));
  assert.equal(accounts.length, 6);
  audit("Account filter passed", {snapshot_mutated: false});
});

test("The model filter requires enabled models with usable accounts; empty results explain the filter", () => {
  const f = fixture();
  f.review.initListFilters();
  const data = {models: [
    {id: "available", enabled: true, availableAccounts: 2},
    {id: "no-accounts", enabled: true, availableAccounts: 0},
    {id: "disabled-model", enabled: false, availableAccounts: 2},
  ]};
  f.review.renderModels(data);
  f.element("onlyUsable").checked = true;
  f.review.changeListFilter("models");
  assert(f.element("modelsTable").innerHTML.includes("available"));
  assert(!f.element("modelsTable").innerHTML.includes("no-accounts"));
  assert(!f.element("modelsTable").innerHTML.includes("disabled-model"));
  f.review.renderModels({models: [{id: "unavailable", availableAccounts: 0}]});
  assert.match(f.element("modelsTable").innerHTML, /uncheck the option to see all models/);
  f.review.renderAccounts([{path: "paused.json", state: "active", schedulingEnabled: false}]);
  f.element("onlyUsableAccounts").checked = true;
  f.review.renderAccounts([{path: "paused.json", state: "active", schedulingEnabled: false}]);
  assert.match(f.element("accountsTable").innerHTML, /uncheck the option to see all accounts/);
  audit("Model filter and empty results passed");
});

test("Two Cookies each remember their state; refresh and re-login lose nothing and no credentials enter Cookies", async () => {
  const jar = new Map();
  const f = fixture(async () => response(204, {}), jar);
  f.review.initListFilters();
  f.review.state.apiKey = "synthetic-private-admin-key";
  f.element("onlyUsableAccounts").checked = true;
  f.review.changeListFilter("accounts");
  f.element("onlyUsable").checked = false;
  f.review.changeListFilter("models");
  assert.equal(jar.get("gateway_accounts_only_usable"), "1");
  assert.equal(jar.get("gateway_models_only_usable"), "0");
  assert.equal(jar.size, 2);
  for (const value of f.cookieWrites) {
    assert.match(value, /Path=\/ui; Max-Age=31536000; SameSite=Lax/);
    assert(!value.includes("synthetic-private-admin-key"));
  }
  f.review.lockDashboard();
  assert.equal(jar.get("gateway_accounts_only_usable"), "1");
  const reloaded = fixture(undefined, jar);
  reloaded.review.initListFilters();
  assert.equal(reloaded.element("onlyUsableAccounts").checked, true);
  assert.equal(reloaded.element("onlyUsable").checked, false);
  reloaded.element("onlyUsable").checked = true;
  reloaded.review.changeListFilter("models");
  reloaded.element("onlyUsableAccounts").checked = false;
  reloaded.review.changeListFilter("accounts");
  const secondReload = fixture(undefined, jar);
  secondReload.review.initListFilters();
  assert.equal(secondReload.element("onlyUsableAccounts").checked, false);
  assert.equal(secondReload.element("onlyUsable").checked, true);
  assert.equal(secondReload.local.size, 0);
  audit("Cookie independent memory and refresh recovery passed", {secret_cookie: false, saved_to_localStorage: false});
});

test("Missing or invalid Cookies default to showing everything; HTTPS adds the Secure attribute", () => {
  const f = fixture(undefined, new Map([
    ["gateway_accounts_only_usable", "true"],
    ["gateway_models_only_usable", "invalid"],
  ]));
  f.review.initListFilters();
  assert.equal(f.element("onlyUsableAccounts").checked, false);
  assert.equal(f.element("onlyUsable").checked, false);
  f.window.location.protocol = "https:";
  f.element("onlyUsable").checked = true;
  f.review.changeListFilter("models");
  assert.match(f.cookieWrites[0], /; Secure$/);
  audit("Invalid Cookie fallback and HTTPS attribute passed");
});

test("With Cookies disabled the filter still applies immediately with a notice that refresh cannot remember; the audit reuses the same TraceID", () => {
  const calls = [];
  const f = fixture(async (route, options) => {
    calls.push({route, options});
    return response(204, {});
  });
  Object.defineProperty(f.document, "cookie", {
    get() { throw new Error("cookie blocked"); },
    set() { throw new Error("cookie blocked"); },
  });
  f.review.initListFilters();
  f.review.state.apiKey = "synthetic-private-admin-key";
  f.review.renderStatus({gateway: {}, accounts: [
    {path: "available.json", state: "active"},
    {path: "paused.json", state: "active", schedulingEnabled: false},
  ]});
  f.element("onlyUsableAccounts").checked = true;
  f.review.changeListFilter("accounts");
  assert(!f.element("accountsTable").innerHTML.includes("paused.json"));
  assert.match(f.element("accountFilterStatus").textContent, /did not allow saving a Cookie/);
  assert(!f.element("accountFilterStatus").classList.contains("hidden"));
  assert.equal(calls.length, 2);
  const records = calls.map(c => JSON.parse(c.options.body));
  assert.deepEqual(records.map(r => r.event), ["list_filter_changed", "list_filter_cookie_failed"]);
  assert(records.every(r => r.enabled === true && r.route === "/admin/api/status"));
  assert.equal(calls[0].options.headers["X-Trace-ID"], calls[1].options.headers["X-Trace-ID"]);
  assert(calls.every(c => !c.options.body.includes("synthetic-private-admin-key")));
  audit("Cookie-disabled fallback and audit passed");
});

test("Switching filters while a request is in flight still renders per the latest checkbox state without extra fetches", async () => {
  let release;
  const calls = [];
  const f = fixture((route, options) => {
    calls.push({route, options});
    if (route === "/admin/api/client-events") return Promise.resolve(response(204, {}));
    return new Promise(resolve => { release = resolve; });
  });
  f.review.initListFilters();
  f.review.state.apiKey = "synthetic-admin-key";
  f.review.state.activeTab = "models";
  const pending = f.review.refreshActive();
  f.element("onlyUsable").checked = true;
  f.review.changeListFilter("models");
  release(response(200, {models: [{id: "available", availableAccounts: 2}, {id: "empty", availableAccounts: 0}]}));
  await pending;
  assert(f.element("modelsTable").innerHTML.includes("available"));
  assert(!f.element("modelsTable").innerHTML.includes("empty"));
  assert.equal(calls.filter(c => c.route === "/admin/api/models?range=5h").length, 1);
  audit("In-flight request renders with latest filter passed", {additional_data_requests: 0});
});

test("Static page copy removed the old brand while technical auth headers stay compatible", () => {
  const html = fs.readFileSync(path.join(projectRoot, "web", "index.html"), "utf8");
  assert(!/workbuddy/i.test(html));
  assert.match(html, /<title>Local Gateway · Web Console<\/title>/);
  assert.match(html, /id="onlyUsableAccounts"/);
  assert.match(html, /Only show usable accounts/);
  assert.match(html, /Only show usable models/);
  const script = fs.readFileSync(path.join(projectRoot, "web", "app.js"), "utf8");
  assert.match(script, /X-Workbuddy-Admin/);
  audit("De-branded copy and protocol compatibility passed");
});

function rateFixture(payload) {
  const calls = [];
  const f = fixture(async (route, options) => {
    calls.push({route, options});
    if (route === "/admin/api/client-events") return response(204, {});
    if (route === "/admin/api/rate-detail") return response(200, payload);
    return response(200, {});
  });
  f.review.state.apiKey = "synthetic-admin-key";
  return {...f, calls};
}

function fakeRateButton(site, model) {
  const details = {open: false, handler: null,
    addEventListener(type, fn) { if (type === "toggle") this.handler = fn; }};
  const panel = {innerHTML: "", className: "rate-detail hidden",
    querySelector(sel) { return sel === "details.rate-raw" ? details : null; },
    classList: {add(n) { this.owner.className += " " + n; }, remove(n) {
      this.owner.className = this.owner.className.split(/\s+/).filter(v => v !== n).join(" ");
    }, owner: null}};
  panel.classList.owner = panel;
  const cell = {querySelector: sel => sel === ".rate-detail" ? panel : null};
  const button = {
    attrs: {"data-rate-site": site, "data-rate-model": model, "aria-expanded": "false"},
    getAttribute(name) { return this.attrs[name] === undefined ? null : this.attrs[name]; },
    setAttribute(name, value) { this.attrs[name] = value; },
    closest(sel) { return sel === "td" ? cell : (sel === "button.rate-info" ? this : null); },
  };
  return {button, panel, details};
}

test("The multiplier cell renders an exclamation button after the value; quotes in model names never break attributes", () => {
  const f = rateFixture({rates: {}});
  const html = f.review.rateCell("intl", "hy4-preview", "Paid (unknown multiplier)");
  assert.match(html, /class='rate-value'>Paid \(unknown multiplier\)</);
  assert.match(html, /class='rate-info'/);
  assert.match(html, /data-rate-site='intl'/);
  assert.match(html, /data-rate-model='hy4-preview'/);
  assert.match(html, /aria-expanded='false'/);
  assert.match(html, /class='rate-detail hidden'/);
  assert.equal(f.review.rateKey("intl", "  HY4-Preview "), "intl|hy4-preview");
  f.review.renderModels({models: [{id: "hy4-preview", cnMultiplier: "0.29x", intlMultiplier: "Paid (unknown multiplier)"}]});
  const table = f.element("modelsTable").innerHTML;
  assert.equal((table.match(/class='rate-info'/g) || []).length, 2);
  assert(table.includes("0.29x") && table.includes("Paid (unknown multiplier)"));
  audit("Multiplier exclamation button rendering passed");
});

test("Known multipliers use plain-language explanations, unknown ones add how-to-refresh, and raw logs stay collapsed by default", () => {
  const f = rateFixture({rates: {}});
  const rawLine = "2026/10/13 10:59:36 [Models] official multiplier site=cn model=hy3 effective=0.29 note=a very long single line of raw log";
  const known = f.review.rateDetailHTML({rates: {"cn|hy3": {
    site: "cn", model: "hy3", source: "Official catalog (live API)", updatedAt: 1791860376,
    value: "0.29x", explain: "Catalog multiplier from the official API", unknown: false, hasLog: true,
    records: [{source: "Official catalog (live API)", updatedAt: 1791860376, value: "0.29x",
      explain: "Catalog multiplier from the official API", logLine: rawLine}],
  }}}, "cn", "hy3");
  assert.match(known, /Source: Official catalog \(live API\)/);
  assert.match(known, /Updated: /);
  assert.match(known, /Catalog multiplier from the official API/);
  assert(!known.includes("How to refresh"), "Known multipliers need no refresh guidance");
  assert.match(known, /Raw logs \(1 lines?, collapsed by default\)/);
  const rawAt = known.indexOf("<code>");
  const detailsAt = known.indexOf("<details");
  const detailsEnd = known.indexOf("</details>");
  assert(detailsAt >= 0 && rawAt > detailsAt && rawAt < detailsEnd, "Raw logs must be inside the collapsible block");
  const unknown = f.review.rateDetailHTML({rates: {"intl|hy4-preview": {
    site: "intl", model: "hy4-preview", source: "Price probe (cached conclusion)", updatedAt: 1791860376,
    value: "Paid (unknown multiplier)", explain: "Last real probe conclusion: paid (not retested yet)",
    refreshHint: "Background automatic probing is off. Request this model once with an account of that site; the gateway will re-judge automatically from the returned usage.credit and write a log.",
    unknown: true, hasLog: true,
    records: [{source: "Price probe (cached conclusion)", updatedAt: 1791860376, value: "Paid (unknown multiplier)",
      explain: "Last real probe conclusion: paid (not retested yet)", logLine: rawLine}],
    others: [{source: "Official catalog (live API)", updatedAt: 1791860370, explain: "The official promo ended on 2026-09-08; the API still returns 0.00x, so that value is untrustworthy"}],
  }}}, "intl", "hy4-preview");
  assert(!unknown.includes("Status: unknown"), "A measured conclusion must not be labeled unknown");
  assert.match(unknown, /Last real probe conclusion: paid \(not retested yet\)/);
  assert.match(unknown, /How to refresh: /);
  assert.match(unknown, /Request this model once/);
  assert.match(unknown, /Other records/);
  assert.match(unknown, /official promo ended on 2026-09-08/);
  const missing = f.review.rateDetailHTML({rates: {}, noLogHint: "There is no record for this model in the regular logs. Request this model once with an account of that site and a new multiplier record will be created."}, "intl", "not-logged-model");
  assert.match(missing, /No logs/);
  assert.match(missing, /Request this model once/);
  assert(!missing.includes("Source:"));
  audit("Multiplier plain-language explanation and refresh guidance passed");
});

test("Clicking the exclamation button fetches log details on demand and reuses the cache; failures never fabricate a source", async () => {
  const payload = {rates: {"cn|hy3": {site: "cn", model: "hy3", source: "Official catalog (live API)",
    updatedAt: 1791860376, value: "0.29x", unknown: false, hasLog: true, records: []}}};
  const f = rateFixture(payload);
  const first = fakeRateButton("cn", "hy3");
  f.review.toggleRateDetail({target: first.button});
  assert.equal(first.button.getAttribute("aria-expanded"), "true");
  assert.equal(f.review.state.openRateKeys["cn|hy3"], true);
  await new Promise(resolve => setImmediate(resolve));
  assert.match(first.panel.innerHTML, /Source: Official catalog/);
  assert.equal(f.calls.filter(c => c.route === "/admin/api/rate-detail").length, 1);
  // Expanding another multiplier on the same page must not request the logs again.
  const second = fakeRateButton("intl", "hy4-preview");
  f.review.toggleRateDetail({target: second.button});
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(f.calls.filter(c => c.route === "/admin/api/rate-detail").length, 1);
  assert.match(second.panel.innerHTML, /No logs/);
  // Click again to collapse and clear the expanded state.
  f.review.toggleRateDetail({target: first.button});
  assert.equal(first.button.getAttribute("aria-expanded"), "false");
  assert.equal(f.review.state.openRateKeys["cn|hy3"], undefined);
  assert.equal(first.panel.innerHTML, "");
  audit("Multiplier detail on-demand fetch and cache passed", {rate_detail_requests: 1});
});

test("After polling rebuilds the table, expanded multiplier details are restored automatically", async () => {
  const payload = {rates: {"cn|hy3": {site: "cn", model: "hy3", source: "Official catalog (live API)",
    updatedAt: 1791860376, value: "0.29x", unknown: false, hasLog: true, records: []}}};
  const f = rateFixture(payload);
  f.review.state.openRateKeys["cn|hy3"] = true;
  f.review.ensureRateDetails();
  await new Promise(resolve => setImmediate(resolve));
  const buttons = [];
  const table = {querySelectorAll: () => buttons};
  const cell = {querySelector: sel => sel === ".rate-detail" ? panel : null};
  const panel = {innerHTML: "", className: "rate-detail hidden",
    querySelector: () => null,
    classList: {add(n) { this.owner.className += " " + n; }, remove(n) {
      this.owner.className = this.owner.className.split(/\s+/).filter(v => v !== n).join(" ");
    }, owner: null}};
  panel.classList.owner = panel;
  const button = {
    attrs: {"data-rate-site": "cn", "data-rate-model": "hy3", "aria-expanded": "false"},
    getAttribute(name) { return this.attrs[name]; },
    setAttribute(name, value) { this.attrs[name] = value; },
    closest(sel) { return sel === "td" ? cell : null; },
  };
  buttons.push(button);
  f.review.restoreRateDetails(table);
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(button.getAttribute("aria-expanded"), "true");
  assert.match(panel.innerHTML, /Source: Official catalog/);
  audit("Details restored after polling passed");
});

test("After expanding raw logs, polling refreshes never auto-collapse them", async () => {
  const rawLine = "2026/10/13 15:28:00 [Models] official multiplier site=cn model=hy3 effective=0.29";
  const payload = {rates: {"cn|hy3": {site: "cn", model: "hy3", source: "Official catalog (live API)",
    updatedAt: 1791860376, value: "0.29x", explain: "Catalog multiplier from the official API", unknown: false, hasLog: true,
    records: [{source: "Official catalog (live API)", updatedAt: 1791860376, value: "0.29x",
      explain: "Catalog multiplier from the official API", logLine: rawLine}]}}};
  const f = rateFixture(payload);
  const {button, details} = fakeRateButton("cn", "hy3");
  f.review.toggleRateDetail({target: button});
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(typeof details.handler, "function", "A toggle listener must be bound after rendering");
  details.open = true;
  details.handler();
  assert.equal(f.review.state.openRawLogs["cn|hy3"], true);
  // Polling rebuilds the entire table; the rebuilt HTML must stay expanded.
  assert.match(f.review.rateDetailHTML(payload, "cn", "hy3"), /<details class='rate-raw' open>/);
  details.open = false;
  details.handler();
  assert.equal(f.review.state.openRawLogs["cn|hy3"], undefined);
  assert(!/<details class='rate-raw' open>/.test(f.review.rateDetailHTML(payload, "cn", "hy3")));
  audit("Raw log expanded state preserved across polling passed");
});

// ---- Overview · usage and performance ------------------------------------------------------

function statsBucket(label, over = {}) {
  return Object.assign({start: 0, label, requests: 0, success: 0, failed: 0, tokens: 0, inputTokens: 0,
    outputTokens: 0, cachedTokens: 0, tokenSamples: 0, credits: 0, creditSamples: 0, ttftSamples: 0,
    avgLatencyMs: null, avgTtftMs: null}, over);
}

function chartModel(html) {
  const attr = html.match(/data-chart-model="([^"]*)"/);
  assert(attr, "expected SVG chart geometry");
  return JSON.parse(attr[1].replace(/&quot;/g, '"').replace(/&#39;/g, "'")
    .replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&amp;/g, "&"));
}

function hoverChart(f, id, rect) {
  const container = f.element(id), model = chartModel(container.innerHTML), listeners = {};
  const shared = id === "usageChart" || id === "latencyChart";
  const group = shared ? f.element("statsCharts") : null;
  if (group && !group.testHoverListeners) {
    group.testHoverListeners = {};
    const add = group.addEventListener;
    group.addEventListener = (name, fn) => { group.testHoverListeners[name] = fn; add.call(group, name, fn); };
  }
  const metrics = {rectReads: 0, attributeWrites: 0, tooltipWrites: 0, sizeReads: 0};
  const cross = {attributes: {}, setAttribute(k, v) { metrics.attributeWrites++; this.attributes[k] = v; }};
  const dots = model.buckets[0].lines.map(() => ({attributes: {},
    setAttribute(k, v) { metrics.attributeWrites++; this.attributes[k] = v; }}));
  let html = "";
  const tooltip = {hidden: true, style: {},
    get innerHTML() { return html; }, set innerHTML(value) { metrics.tooltipWrites++; html = value; },
    get offsetWidth() { metrics.sizeReads++; return 210; }, get offsetHeight() { metrics.sizeReads++; return 120; }};
  const area = {addEventListener(name, fn) { listeners[name] = fn; }};
  const svg = {getAttribute: () => JSON.stringify(model),
    querySelector: selector => selector === ".chart-hover-area" ? area : cross,
    querySelectorAll: () => dots, getBoundingClientRect: () => { metrics.rectReads++; return rect; }};
  container.querySelector = selector => selector === "svg" ? svg : tooltip;
  container.clientWidth = rect.width;
  container.clientHeight = rect.height;
  f.review.bindStatsChartHover(container);
  // The real shared-chart listener is delegated to the parent, so emulate
  // bubbling in this DOM stub rather than calling an obsolete plot listener.
  if (shared) {
    listeners.pointermove = event => group.testHoverListeners.pointermove(event);
    listeners.pointerleave = () => {};
    listeners.pointercancel = event => group.testHoverListeners.pointercancel(event);
  }
  return {model, listeners, tooltip, cross, dots, metrics,
    move(index, y) {
      listeners.pointermove({clientX: rect.left + model.buckets[index].x * rect.width / model.width,
        clientY: rect.top + (y === undefined ? rect.height / 2 : y)});
    }};
}

function animationClock(f, cancellable = true) {
  let id = 0;
  const callbacks = new Map();
  f.window.requestAnimationFrame = callback => { callbacks.set(++id, callback); return id; };
  if (cancellable) f.window.cancelAnimationFrame = frame => callbacks.delete(frame);
  return {get pending() { return callbacks.size; }, flush() {
    const queued = [...callbacks.values()];
    callbacks.clear();
    queued.forEach(callback => callback());
  }};
}

function statsPayload(over = {}) {
  return Object.assign({
    period: "day", account: "", timezone: "Asia/Shanghai", ttftMetric: "first_token_v2", start: "2026-09-15", end: "2026-10-14",
    startedAt: 1760000000000, accounts: [{id: "acc-a", file: "workbuddy-a.json", edition: "cn"}],
    summary: {requests: 2, success: 1, failed: 1, tokens: 300, inputTokens: 200, outputTokens: 100,
      cachedTokens: 50, tokenSamples: 1, credits: 1.25, creditSamples: 1, ttftSamples: 1,
      avgLatencyMs: 1200, avgTtftMs: 300},
    buckets: [statsBucket("2026-10-13", {requests: 1, success: 1, tokens: 300, inputTokens: 200,
      outputTokens: 100, cachedTokens: 50, tokenSamples: 1, credits: 1.25, creditSamples: 1,
      ttftSamples: 1, avgLatencyMs: 1200, avgTtftMs: 300}), statsBucket("2026-10-14")],
    models: [
      {model: "deepseek-v4.1-flash", requests: 3, success: 3, failed: 0, tokens: 900000,
        inputTokens: 700000, outputTokens: 200000, cachedTokens: 100000, tokenSamples: 3,
        credits: 4.5, creditSamples: 3, ttftSamples: 3, avgLatencyMs: 1500, avgTtftMs: 400},
      {model: "hy3", requests: 1, success: 1, failed: 0, tokens: 0, inputTokens: 0, outputTokens: 0,
        cachedTokens: 0, tokenSamples: 0, credits: 0, creditSamples: 0, ttftSamples: 1,
        avgLatencyMs: 800, avgTtftMs: 300},
    ],
    modelLatency: [
      {model: "deepseek-v4.1-flash", avgLatencyMs: [1500, null], avgTtftMs: [400, null]},
      {model: "hy3", avgLatencyMs: [null, 800], avgTtftMs: [null, 300]},
    ],
    storage: {mode: "file", message: "", lastSavedAt: 1760000000000},
  }, over);
}

test("Stats polling reuses charts when only the server clock changes; crossing a future-bucket boundary, data or width changes still redraw", () => {
  const f = fixture(), chart = f.element("usageChart");
  let markup = "", writes = 0;
  Object.defineProperty(chart, "innerHTML", {configurable: true,
    get: () => markup, set: value => { markup = value; writes++; }});
  const start = Date.parse("2026-10-27T08:00:00+08:00");
  const data = statsPayload({serverTime: start + 1000, buckets: [
    statsBucket("08:00", {start, requests: 1, tokens: 100, tokenSamples: 1}),
    statsBucket("09:00", {start: start + 3600000}),
  ]});
  f.review.renderStats(data);
  f.review.renderStats({...data, serverTime: start + 2000});
  assert.equal(writes, 1, "Server time advancing per second must not clear the tooltip and rebuild the SVG");
  f.review.renderStats({...data, serverTime: start + 2000,
    storage: {lastSavedAt: start + 2000, message: "Synthetic storage warning"}});
  assert.equal(writes, 1, "A changed persistence timestamp must not rebuild the chart");
  assert.equal(f.element("statsInfo").textContent, "Synthetic storage warning");
  f.review.renderStats({...data, serverTime: start + 3600001});
  assert.equal(writes, 2, "Former future buckets must re-display when their time comes; the cache must not freeze them");
  chart.clientWidth = 1200;
  f.element("latencyChart").clientWidth = 1200;
  f.review.renderStats({...data, serverTime: start + 3600001});
  assert.equal(writes, 3, "A window change must update the viewBox");
  const changed = {...data, serverTime: start + 3600001,
    summary: {...data.summary, tokens: 1000}};
  f.review.renderStats(changed);
  assert.equal(writes, 4, "Real stats changes must not be swallowed by the cache");
  f.review.lockDashboard();
  f.review.renderStats(changed);
  assert.equal(writes, 5, "After logout clears, re-rendering must be allowed");
  audit("Stats identical data reuse passed", {clock_only_skipped: true, future_boundary_redraw: true,
    width_and_data_redraw: true, logout_resets_cache: true});
});

test("Background stats reads insert no temporary notice and never overwrite a displayed storage warning", async () => {
  let finish;
  const payload = statsPayload({storage: {mode: "memory", message: "Stats cannot be persisted right now; in-memory data is kept"}});
  const f = fixture(async route => route.startsWith("/admin/api/stats?") ?
    new Promise(resolve => { finish = () => resolve(response(200, payload)); }) : response(204, {}));
  f.review.state.apiKey = "synthetic-stats-key";
  f.review.renderStats(payload);
  const before = f.element("statsInfo").textContent;
  const loading = f.review.loadStats(false);
  assert.equal(f.element("statsInfo").textContent, before);
  finish();
  await loading;
  assert.equal(f.element("statsInfo").textContent, before);
  audit("Stats silent refresh keeps layout passed", {temporary_loading_row_removed: true, storage_warning_preserved: true});
});

test("TTFT old/new metric isolation: legacy snapshots are not passed off as first token and excluded counts are shown explicitly", () => {
  const f=fixture();
  const old=statsPayload({ttftMetric: undefined});
  old.summary.avgTtftMs=87190;
  f.review.renderStats(old);
  assert(!f.element("usageSummary").innerHTML.includes("87.19"));
  assert.match(f.element("usageSummary").innerHTML,/Avg first token/);
  assert.match(f.element("latencyLegend").innerHTML, /deepseek-v4\.1-flash/);
  const oldReadout = chartModel(f.element("latencyChart").innerHTML).buckets[0].readout;
  assert.equal(oldReadout.find(r => r.label === "Avg first token").display, "—", "The shared tooltip must not mix in the legacy TTFT metric");
  const current=statsPayload();
  current.summary.legacyTtftSamples=12;
  f.review.renderStats(current);
  assert.equal(f.element("statsInfo").textContent, "", "The long stats-metric copy has been removed");
  assert.match(f.element("usageSummary").innerHTML,/0\.30 s/);
  f.review.renderModels({models:[{id:"legacy",avgTtftMs:87190,hasTtft:true}]});
  assert(!f.element("modelsTable").innerHTML.includes("87.19"));
  audit("First-token old/new sample isolation passed",{legacy_samples_excluded:true});
});

test("The stats summary distinguishes unknown from zero; the persistence badge is gone but memory anomalies still warn", () => {
  const f = fixture();
  f.review.renderStats(statsPayload());
  const cards = f.element("usageSummary").innerHTML;
  assert.match(cards, /Total tokens/);
  assert.match(cards, /1\.25/, "Credits must be shown from actual usage.credit");
  assert.match(cards, /1\/2 requests have credits/, "The credit sample coverage count must be exposed");
  // Show unknown when there are no samples, not 0.
  f.review.renderStats(statsPayload({summary: {requests: 1, success: 1, failed: 0, tokenSamples: 0,
    creditSamples: 0, ttftSamples: 0, avgLatencyMs: 900, avgTtftMs: null}}));
  const unknown = f.element("usageSummary").innerHTML;
  assert.match(unknown, /Total tokens<\/div><div class="usage-value">—</, "No token samples must not display as 0");
  assert.match(unknown, /Total credits<\/div><div class="usage-value">—</, "No credit samples must not display as 0");
  // Memory-only mode must state it explicitly so it is not mistaken for persisted data.
  f.review.renderStats(statsPayload({storage: {mode: "memory", message: "Stats cannot be persisted right now; still counting in memory"}}));
  assert.equal(f.element("statsStorage").textContent, "", "The stats persistence badge block has been deleted");
  assert.match(f.element("statsInfo").textContent, /cannot be persisted right now/);
  audit("Stats summary and persistence status passed", {unknown_not_zero: true, memory_mode_flagged: true});
});

test("The latency line bridges empty buckets to connect adjacent samples and never fakes zero values", () => {
  const f = fixture();
  const svg = f.review.statsLineChart([
    statsBucket("2026-10-12", {avgLatencyMs: 1000, avgTtftMs: 200}),
    statsBucket("2026-10-13"),
    statsBucket("2026-10-14", {avgLatencyMs: 2000, avgTtftMs: 400}),
  ], [
    {label: "Avg total latency", color: "token", value: b => b.avgLatencyMs},
    {label: "Avg first token", color: "first", value: b => b.avgTtftMs},
  ], "Latency", v => String(v));
  assert.match(svg, /<svg /);
  // A middle bucket without samples still yields one continuous line, but empty buckets draw no dots and never fake 0ms.
  assert.equal((svg.match(/class="chart-line series-token"/g) || []).length, 1);
  assert.equal((svg.match(/class="chart-line series-first"/g) || []).length, 1);
  assert.equal((svg.match(/class="chart-line/g) || []).length, 2, "Empty buckets should be bridged, not broken");
  assert.equal((svg.match(/<circle class="series-token"/g) || []).length, 2, "Empty buckets must not fake line points");
  assert.match(svg, /data-chart-model=/, "The tooltip layer must carry geometry data");
  assert.equal(f.review.statsLineChart([statsBucket("2026-10-14")], [
    {label: "Avg total latency", color: "token", value: b => b.avgLatencyMs},
  ], "Latency", String), '<p class="stats-empty">No samples available for this time range</p>', "All-empty must show the empty state");
  audit("Latency line bridging and empty state passed", {no_fake_zero: true, connected_paths: 2});
});

test("Request latency draws one line per model instead of blending all models into a single average line", () => {
  const f = fixture();
  f.review.renderStats(statsPayload());
  const svg = f.element("latencyChart").innerHTML;
  const legend = f.element("latencyLegend").innerHTML;
  assert(legend.indexOf("deepseek-v4.1-flash") < legend.indexOf("hy3"), "The legend keeps the server order");
  assert.equal((svg.match(/class="chart-line series-m0"/g) || []).length, 1);
  assert.equal((svg.match(/class="chart-line series-m1"/g) || []).length, 1);
  assert.equal((svg.match(/class="chart-line /g) || []).length, 2, "One line per model");
  assert(svg.includes("deepseek-v4.1-flash"), "Model names must enter the tooltip data");
  assert(svg.includes("1.50 s"), "Latency values must enter the tooltip data");
  assert(svg.includes("hy3") && svg.includes("0.80 s"));
  assert(!svg.includes("series-token"), "A blended all-model average line must no longer be drawn");
  f.review.renderStats(statsPayload({modelLatency: []}));
  assert.match(f.element("latencyChart").innerHTML, /No samples available for this time range/);
  assert.equal(f.element("latencyLegend").textContent, "");
  audit("Per-model latency lines passed", {models: 2, blended_line: false});
});

test("The model usage ranking renders in server order and follows the account selection", () => {
  const f = fixture();
  f.review.renderStats(statsPayload({account: "acc-a"}));
  const rows = f.element("statsTable").querySelector("tbody").innerHTML;
  assert.match(rows, /deepseek-v4\.1-flash/);
  assert.match(rows, /hy3/);
  // The server already sorted by usage; the front end must keep that order and never re-sort.
  assert(rows.indexOf("deepseek-v4.1-flash") < rows.indexOf("hy3"), "Higher-usage models must rank first");
  assert.match(rows, /<td>1<\/td><td class='mono'>deepseek/, "Rank numbers must be shown");
  assert.match(rows, /Token 0\/1/, "Models without usage must still be listed with their coverage count");
  assert(!/<td>\d{4}-\d{2}-\d{2}<\/td>/.test(rows), "The breakdown must no longer output a time column");
  const select = f.element("statsAccount").innerHTML;
  assert.match(select, /Global · All accounts/);
  assert.match(select, /workbuddy-a\.json · Domestic/);
  assert.equal(f.element("statsAccount").value, "acc-a", "The current selection must be preserved");
  assert.equal(f.element("statsStart").value, "2026-09-15");
  assert.equal(f.element("statsEnd").value, "2026-10-14");
  // An empty range shows an explicit empty state instead of an empty table. Placed last to avoid resetting the selection assertions above.
  f.review.renderStats(statsPayload({models: []}));
  assert.match(f.element("statsTable").querySelector("tbody").innerHTML, /No model calls in the selected range/);
  audit("Model usage ranking rendering passed", {server_order_kept: true, no_time_column: true, empty_state: true});
});

// Regression: when stats requests slow down or hang, status polling must still complete and never be dragged down by stats.
test("Stats loading does not block status polling", async () => {
  let releaseStatus;
  const routes = [];
  const f = fixture((route, options) => {
    routes.push(route);
    if (route === "/admin/api/client-events") return Promise.resolve(response(204, {}));
    if (route.startsWith("/admin/api/stats")) return new Promise(() => {}); // never resolves
    return new Promise(resolve => { releaseStatus = resolve; });
  });
  f.review.state.apiKey = "synthetic-admin-key";
  f.review.state.activeTab = "overview";
  const pending = f.review.refreshActive();
  releaseStatus(response(200, {gateway: {}, accounts: [], summary: {total: 0}}));
  await pending;
  assert.equal(f.review.state.refreshPending, false, "Stats not returning must never leave polling stuck pending");
  assert(routes.some(r => r.startsWith("/admin/api/stats")), "The overview should trigger one stats request");
  audit("Stats not blocking polling passed", {stats_pending_isolated: true});
});

test("The usage trend merges into one chart: token line plus credit bars on dual left/right axes", () => {
  const f = fixture();
  f.review.renderStats(statsPayload());
  const svg = f.element("usageChart").innerHTML;
  assert.match(svg, /class="chart-line series-token"/, "Total tokens must be drawn as a line");
  assert.match(svg, /class="chart-bar series-credit"/, "Credits must be drawn as bars");
  assert.equal((svg.match(/class="chart-line series-credit"/g) || []).length, 0, "Credits must no longer be drawn as a line");
  assert.equal((svg.match(/class="chart-bar series-token"/g) || []).length, 0, "Tokens must no longer be drawn as bars");
  assert((svg.match(/class="chart-label"/g) || []).length >= 10, "Token and credit scales differ, so dual left/right axes are required");
  assert(!/NaN|undefined/.test(svg), "The chart must never contain NaN or undefined");
  audit("Merged usage chart passed", {token_line: true, credit_bar: true, dual_axis: true});
});

test("Latency trend: first token as bars and total latency as lines, both split per model", () => {
  const f = fixture();
  f.review.renderStats(statsPayload());
  const svg = f.element("latencyChart").innerHTML;
  assert.equal((svg.match(/class="chart-line series-m0"/g) || []).length, 1, "Total latency remains a line");
  assert.equal((svg.match(/class="chart-line series-m1"/g) || []).length, 1);
  assert.equal((svg.match(/class="chart-bar series-m0"/g) || []).length, 1, "First token must be bars");
  assert.equal((svg.match(/class="chart-bar series-m1"/g) || []).length, 1);
  assert.equal((svg.match(/class="chart-line /g) || []).length, 2, "Line count equals model count");
  assert.equal((svg.match(/class="chart-bar /g) || []).length, 2, "Bar count equals model count");
  assert(!svg.includes("series-token"), "The latency chart must not mix in usage series colors");
  audit("Latency bar-line combination passed", {ttft_bar: true, latency_line: true, per_model: true});
});

test("Dual-axis combo chart: lines on the left axis and bars on the right; missing samples draw no bars", () => {
  const f = fixture();
  const svg = f.review.statsComboChart([
    statsBucket("2026-10-13", {tokens: 2000000, tokenSamples: 1, credits: 20, creditSamples: 1}),
    statsBucket("2026-10-14"),
    statsBucket("2026-10-15", {tokens: 4000000, tokenSamples: 1, credits: 40, creditSamples: 1}),
  ], {
    lines: [{label: "Total tokens", color: "token", value: b => (b.tokenSamples ? b.tokens : null)}],
    bars: [{label: "Credits", color: "credit", value: b => (b.creditSamples ? b.credits : null)}],
    title: "Dual axis", leftFormat: v => (v / 1000000).toFixed(2) + "M",
    rightFormat: v => Number(v).toFixed(2), box: {w: 600, h: 240}
  });
  assert.match(svg, /class="chart-line series-token"/);
  assert.match(svg, /class="chart-bar series-credit"/);
  assert((svg.match(/class="chart-label"/g) || []).length >= 10, "Dual axes need left and right tick sets");
  assert.match(svg, /M<\/text>/, "The left axis uses token units and the right axis uses credit precision");
  assert(!/NaN|undefined/.test(svg), "Dual axes must never contain NaN or undefined");
  assert.equal((svg.match(/class="chart-line series-token"/g) || []).length, 1, "The line should bridge empty buckets");
  assert.equal((svg.match(/class="chart-bar series-credit"/g) || []).length, 2, "A middle bucket without samples draws no bar");
  assert.match(svg, /class="chart-crosshair"/, "A crosshair is required");
  assert.match(svg, /class="chart-hover-area"/, "A mouse hit layer is required");
  assert.match(svg, /class="chart-tooltip"/, "A tooltip readout layer is required");
  assert(!f.review.statsComboChart([statsBucket("2026-10-14")], {
    lines: [], bars: [{label: "Credits", color: "credit", value: b => (b.creditSamples ? b.credits : null)}],
    title: "Empty", leftFormat: String, box: {w: 600, h: 240}
  }).includes("<svg"), "All-empty must show the empty state");
  audit("Dual-axis combo chart passed", {gap_connected: true, dual_axis: true});
});

test("The usage line connects sample-less buckets at 0 and generates tooltip data", () => {
  const f = fixture();
  const svg = f.review.statsComboChart([
    statsBucket("2026-10-13", {tokens: 100, tokenSamples: 1}),
    statsBucket("2026-10-14"),
    statsBucket("2026-10-15", {tokens: 300, tokenSamples: 1}),
  ], {
    lines: [{label: "Total tokens", color: "token", connectZero: true, value: b => (b.tokenSamples ? b.tokens : null)}],
    bars: [], title: "Zero connection", leftFormat: String, box: {w: 600, h: 240}
  });
  assert.equal((svg.match(/class="chart-line series-token"/g) || []).length, 1);
  assert.equal((svg.match(/<circle class="series-token"/g) || []).length, 3, "Empty buckets must get 0 points");
  assert.match(svg, /class="chart-tooltip"/, "The tooltip layer must exist");
  assert.match(svg, /data-chart-model=/, "The tooltip layer must carry per-bucket dates and values");
  assert(svg.includes("2026-10-14") && svg.includes('&quot;display&quot;:&quot;0&quot;'), "Empty buckets must enter tooltip data as 0");
  audit("Zero connection and tooltip layer passed", {zero_points: 3, tooltip: true});
});

test("The chart tooltip shows the date and per-series readouts at the mouse position", () => {
  const f = fixture();
  const svgText = f.review.statsComboChart([
    statsBucket("2026-10-13", {tokens: 100, tokenSamples: 1, credits: 2, creditSamples: 1}),
    statsBucket("2026-10-14"),
    statsBucket("2026-10-15", {tokens: 300, tokenSamples: 1, credits: 4, creditSamples: 1}),
  ], {
    lines: [{label: "Total tokens", color: "token", connectZero: true, value: b => (b.tokenSamples ? b.tokens : null)}],
    bars: [{label: "Credits", color: "credit", value: b => (b.creditSamples ? b.credits : null)}],
    title: "Tooltip", leftFormat: String, rightFormat: String, box: {w: 600, h: 240}
  });
  const encoded = svgText.match(/data-chart-model="([^"]*)"/)[1]
    .replace(/&quot;/g, '"').replace(/&amp;/g, "&").replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&#39;/g, "'");
  const model = JSON.parse(encoded);
  const listeners = {};
  const cross = {attributes: {}, setAttribute(k, v) { this.attributes[k] = v; }};
  const dots = model.buckets[0].lines.map(() => ({attributes: {}, setAttribute(k, v) { this.attributes[k] = v; }}));
  const tooltip = {hidden: true, innerHTML: "", style: {}, offsetWidth: 180, offsetHeight: 80};
  const area = {addEventListener(name, fn) { listeners[name] = fn; }};
  const svg = {
    getAttribute: () => JSON.stringify(model),
    querySelector: selector => selector === ".chart-hover-area" ? area : cross,
    querySelectorAll: () => dots,
    getBoundingClientRect: () => ({left: 0, top: 0, width: 600, height: 240}),
  };
  const container = {querySelector: selector => selector === "svg" ? svg : tooltip, clientWidth: 600, clientHeight: 240};
  f.review.bindStatsChartHover(container);
  listeners.pointermove({clientX: 357});
  assert.equal(tooltip.hidden, false);
  assert.match(tooltip.innerHTML, /2026-10-14/);
  assert.match(tooltip.innerHTML, /Total tokens/);
  assert.match(tooltip.innerHTML, />0</);
  assert.match(tooltip.innerHTML, /Credits/);
  assert.equal(cross.attributes.visibility, "visible");
  assert.equal(dots[0].attributes.visibility, "visible");
  listeners.pointerleave();
  assert.equal(tooltip.hidden, true);
  assert.equal(cross.attributes.visibility, "hidden");
  audit("Mouse crosshair and per-series readouts passed", {crosshair: true, tooltip: true});
});

test("Both stats charts share the plot area, date ticks, and four summary readouts, including narrow screens and dual-axis scale differences", () => {
  for (const width of [260, 380, 900, 1800]) {
    const f = fixture();
    f.element("usageChart").clientWidth = width;
    f.element("latencyChart").clientWidth = width;
    f.review.renderStats(statsPayload());
    const usage = chartModel(f.element("usageChart").innerHTML);
    const latency = chartModel(f.element("latencyChart").innerHTML);
    assert.equal(usage.width, latency.width);
    assert.equal(usage.left, latency.left);
    assert.equal(usage.plotW, latency.plotW);
    assert(usage.plotW >= 98, "small screens retain a usable plot");
    assert.deepEqual(usage.buckets.map(r => r.x), latency.buckets.map(r => r.x));
    for (let i = 0; i < usage.buckets.length; i++) {
      const row = usage.buckets[i];
      assert.deepEqual(row.readout.map(v => v.label), ["Total tokens", "Credits", "Avg total latency", "Avg first token"]);
      assert.deepEqual(row.readout, latency.buckets[i].readout);
    }
    assert.deepEqual(usage.buckets[0].readout.map(r => r.display), ["300", "1.25", "1.20 s", "0.30 s"]);
    const xLabels = html => [...html.matchAll(/<text class="chart-label" text-anchor="[^"]+" x="([^"]+)" y="[^"]+">([^<]+)<\/text>/g)]
      .filter(m => /10-1[34]/.test(m[2])).map(m => [m[1], m[2]]);
    assert.deepEqual(xLabels(f.element("usageChart").innerHTML), xLabels(f.element("latencyChart").innerHTML));
  }
  audit("Both charts share the time axis and four readouts passed", {responsive_widths: 4, aligned: true, readout_count: 4});
});

test("The shared four readouts distinguish idle buckets from missing usage without changing raw stats", () => {
  const f = fixture(), payload = statsPayload();
  payload.buckets.push(statsBucket("2026-10-15", {requests: 1, avgLatencyMs: 900}));
  const original = JSON.stringify(payload);
  f.review.renderStats(payload);
  const buckets = chartModel(f.element("usageChart").innerHTML).buckets;
  assert.deepEqual(buckets[1].readout.map(r => r.display), ["0", "0", "—", "—"]);
  assert.deepEqual(buckets[2].readout.map(r => r.display), ["—", "—", "0.90 s", "—"]);
  assert.equal(JSON.stringify(payload), original);
  audit("Shared readout empty-sample semantics passed", {idle_zero: true, unknown_usage_preserved: true});
});

test("Future buckets get no zeros and no points, lines, or bars; past zeros are kept, both charts align, and server time is used", () => {
  const f = fixture(), now = Date.parse("2026-10-24T11:30:00+08:00");
  const payload = statsPayload({serverTime: now});
  payload.buckets = ["2026-10-24T09:00:00+08:00", "2026-10-24T10:00:00+08:00",
    "2026-10-24T11:00:00+08:00", "2026-10-24T12:00:00+08:00", "2026-10-25T00:00:00+08:00"]
    .map((at, i) => statsBucket(at, {start: Date.parse(at), ...(i === 0 || i >= 3
      ? {tokens: i ? 1e12 : 300, tokenSamples: 1, credits: i ? 1e9 : 2, creditSamples: 1}
      : {})}));
  payload.modelLatency = [{model: "synthetic-model", avgLatencyMs: [1000, null, 1200, 1e12, 1e12],
    avgTtftMs: [100, null, 120, 1e12, 1e12]}];
  const before = JSON.stringify(payload);
  f.review.renderStats(payload);
  const usage = chartModel(f.element("usageChart").innerHTML);
  const latency = chartModel(f.element("latencyChart").innerHTML);
  assert.deepEqual(usage.buckets.map(b => b.x), latency.buckets.map(b => b.x));
  assert.equal(usage.buckets.length, 5, "future buckets must not be filtered out and shift model indices");
  assert.deepEqual(usage.buckets.map(b => b.lines[0].value), [300, 0, 0, null, null]);
  assert.deepEqual(latency.buckets.map(b => b.lines[0].value), [1000, null, 1200, null, null]);
  for (const model of [usage, latency]) {
    assert.deepEqual(model.buckets.map(b => b.future), [false, false, false, true, true]);
    assert(model.buckets.slice(3).every(b => b.lines.concat(b.bars).every(v => v.value === null && v.y === null)));
    assert(model.buckets.slice(3).every(b => b.readout.length === 4 && b.readout.every(r => r.display === "—")));
  }
  assert.equal((f.element("usageChart").innerHTML.match(/<circle class="series-token"/g) || []).length, 3);
  assert.equal((f.element("usageChart").innerHTML.match(/class="chart-bar series-credit"/g) || []).length, 1);
  assert(!f.element("usageChart").innerHTML.includes("1000000.00M"), "future values must not inflate the scale");
  f.element("statsCharts").getBoundingClientRect = () => ({left: 0, top: 0, width: 900, height: 700});
  const usageHover = hoverChart(f, "usageChart", {left: 0, top: 0, width: 900, height: 280});
  usageHover.move(3);
  assert.match(usageHover.tooltip.innerHTML, /Not yet due/);
  assert.equal((usageHover.tooltip.innerHTML.match(/class="chart-tooltip-row"/g) || []).length, 4);
  assert.equal(usageHover.dots[0].attributes.visibility, "hidden");
  assert.equal(JSON.stringify(payload), before);
  audit("Future bucket blanking passed", {server_clock: true, future_buckets: 2, past_zero_connected: true, aligned: true});
});

test("Future detection works on absolute time for all granularities; the current bucket start boundary is still drawn", () => {
  const f = fixture(), now = Date.parse("2030-01-01T00:00:00+08:00");
  for (const period of ["hour", "day", "week", "month"]) {
    const buckets = [now - 1, now, now + 1].map(start => statsBucket(period, {start, tokens: 7, credits: 3}));
    const html = f.review.statsComboChart(buckets, {
      nowMs: now, lines: [{label: "Token", color: "token", connectZero: true, value: b => b.tokens}],
      bars: [{label: "Credits", color: "credit", value: b => b.credits}], rightFormat: String
    });
    const rows = chartModel(html).buckets;
    assert.deepEqual(rows.map(b => b.future), [false, false, true]);
    assert.deepEqual(rows.map(b => b.lines[0].value), [7, 7, null]);
    assert.deepEqual(rows.map(b => b.bars[0].value), [3, 3, null]);
  }
  const empty = f.review.statsComboChart([statsBucket("future", {start: now + 1})], {
    nowMs: now, lines: [{label: "Token", connectZero: true, value: () => null}]
  });
  assert.match(empty, /has not started yet/);
  assert(!empty.includes("<svg"), "all-future range must not masquerade as measured zero");
  audit("Future bucket boundary passed", {granularities: 4, current_bucket_included: true, all_future_empty: true});
});

test("Legacy stats APIs without serverTime fall back to local time; invalid bucket times do not affect legacy data display", () => {
  const f = fixture(), now = Date.now();
  const html = f.review.statsComboChart([
    statsBucket("old", {start: 0}), statsBucket("invalid", {start: "invalid"}),
    statsBucket("future", {start: now + 86400000})
  ], {lines: [{label: "Token", connectZero: true, value: () => null}]});
  assert.deepEqual(chartModel(html).buckets.map(b => b.lines[0].value), [0, 0, null]);
  audit("Legacy API compatibility passed", {fallback_clock: true, invalid_time_safe: true});
});

test("Hovering either chart shows the four readouts; the guide spans the inter-card gap and logs interaction start/end only once", () => {
  const calls = [];
  const f = fixture(async (route, options) => { calls.push({route, options}); return response(204, {}); });
  f.review.state.apiKey = "synthetic-private-hover-key";
  f.element("usageChart").clientWidth = 900;
  f.element("latencyChart").clientWidth = 900;
  f.review.renderStats(statsPayload());
  const groupEvents = {}, group = f.element("statsCharts"), guide = f.element("statsChartsCrosshair");
  group.getBoundingClientRect = () => ({left: 20, top: 100, width: 900, height: 750});
  group.addEventListener = (name, callback) => { groupEvents[name] = callback; };
  const usage = hoverChart(f, "usageChart", {left: 20, top: 130, width: 900, height: 280});
  const latency = hoverChart(f, "latencyChart", {left: 20, top: 520, width: 900, height: 280});
  usage.move(0);
  assert.equal(guide.hidden, false);
  assert.equal((usage.tooltip.innerHTML.match(/class="chart-tooltip-row"/g) || []).length, 4);
  assert.match(usage.tooltip.innerHTML, /Avg total latency/);
  assert.match(usage.tooltip.innerHTML, /Avg first token/);
  assert.equal(usage.cross.attributes.visibility, "hidden", "one guide should span both cards");
  assert.equal(latency.dots[0].attributes.visibility, "visible", "other chart highlights same bucket");
  assert(Number.parseFloat(guide.style.height) > 390, "guide spans both plots and the inter-card gap");
  const position = usage.tooltip.style.transform;
  usage.move(0, 170);
  assert.notEqual(usage.tooltip.style.transform, position, "tooltip follows pointer even within the same bucket");
  usage.listeners.pointerleave();
  assert.equal(guide.hidden, false, "crossing the gap must not break the guide");
  latency.move(0);
  assert.equal(usage.tooltip.hidden, true, "only the active pane has a tooltip");
  assert.equal(latency.tooltip.hidden, false);
  assert.equal((latency.tooltip.innerHTML.match(/class="chart-tooltip-row"/g) || []).length, 4);
  assert.equal(calls.length, 1, "mousemove must not spam audit requests");
  groupEvents.pointerleave();
  assert.equal(guide.hidden, true);
  assert.equal(latency.tooltip.hidden, true);
  assert.equal(calls.length, 2);
  assert.deepEqual(calls.map(c => JSON.parse(c.options.body).event), ["stats_chart_hover_started", "stats_chart_hover_ended"]);
  assert.equal(calls[0].options.headers["X-Trace-ID"], calls[1].options.headers["X-Trace-ID"]);
  assert(calls.every(c => !c.options.body.includes("synthetic-private-hover-key") && !c.options.body.includes("300")));
  audit("Cross-chart cursor linking and persisted audit passed", {spans_gap: true, one_tooltip: true, trace_reused: true});
});

test("Fast movement coalesces to once per frame; within the same bucket only the tooltip moves, with no content rebuilds or repeated measurements", () => {
  const calls = [], f = fixture(async (route, options) => { calls.push({route, options}); return response(204, {}); });
  const clock = animationClock(f);
  f.review.state.apiKey = "synthetic-performance-key";
  f.review.renderStats(statsPayload());
  let groupReads = 0;
  f.element("statsCharts").getBoundingClientRect = () => {
    groupReads++;
    return {left: 20, top: 100, width: 900, height: 750};
  };
  const usage = hoverChart(f, "usageChart", {left: 20, top: 130, width: 900, height: 280});
  const latency = hoverChart(f, "latencyChart", {left: 20, top: 520, width: 900, height: 280});
  for (let i = 0; i < 1000; i++) usage.move(i % 2, 80 + i % 80);
  assert.equal(clock.pending, 1);
  assert.equal(usage.metrics.rectReads, 0);
  assert.equal(usage.metrics.tooltipWrites, 0);
  clock.flush();
  assert.equal(usage.metrics.rectReads, 1);
  assert.equal(latency.metrics.rectReads, 1);
  assert.equal(groupReads, 1);
  assert.equal(usage.metrics.tooltipWrites, 1);
  assert.equal(usage.metrics.sizeReads, 2);
  assert.equal(latency.metrics.tooltipWrites, 0);
  assert.equal(latency.metrics.sizeReads, 0, "inactive pane never measures its hidden tooltip");
  assert.match(usage.tooltip.innerHTML, /2026-10-14/, "newest pointer wins");
  const before = {...usage.metrics}, otherBefore = {...latency.metrics}, position = usage.tooltip.style.transform;
  for (let i = 0; i < 500; i++) usage.move(1, 190);
  clock.flush();
  assert.equal(usage.metrics.tooltipWrites, before.tooltipWrites);
  assert.equal(usage.metrics.sizeReads, before.sizeReads);
  assert.equal(usage.metrics.attributeWrites, before.attributeWrites);
  assert.equal(latency.metrics.attributeWrites, otherBefore.attributeWrites);
  assert.notEqual(usage.tooltip.style.transform, position);
  assert.equal(calls.length, 1, "audit still records one start, not 1500 pointer events");
  usage.move(0);
  latency.move(1);
  assert.equal(clock.pending, 1, "both panes share a single animation queue");
  clock.flush();
  assert.equal(usage.tooltip.hidden, true);
  assert.equal(latency.tooltip.hidden, false, "latest pane wins too");
  assert.match(latency.tooltip.innerHTML, /2026-10-14/);
  latency.listeners.pointercancel();
  assert.equal(calls.length, 2);
  assert.equal(calls[0].options.headers["X-Trace-ID"], calls[1].options.headers["X-Trace-ID"]);
  audit("Per-frame hover coalescing and caching passed", {pointer_events: 1500, painted_frames: 2, same_bucket_dom_rebuilds: 0,
    inactive_tooltip_reads: 0, shared_queue: true, audit_events: 2});
});

test("The dual-chart pane parent covers the x-axis, title, card padding, and inter-card gap, still reading layout once per frame", () => {
  const f = fixture(), clock = animationClock(f);
  f.review.renderStats(statsPayload());
  const group = f.element("statsCharts");
  let groupReads = 0;
  group.getBoundingClientRect = () => { groupReads++; return {left: 20, top: 100, width: 900, height: 750}; };
  const usage = hoverChart(f, "usageChart", {left: 20, top: 130, width: 900, height: 280});
  const latency = hoverChart(f, "latencyChart", {left: 20, top: 520, width: 900, height: 280});
  for (const [region, y, active] of [
    ["upper-title", 105, usage], ["upper-axis", 400, usage], ["upper-padding", 420, usage],
    ["gap-near-upper", 450, usage], ["gap-near-lower", 490, latency],
    ["lower-title", 510, latency], ["lower-axis", 790, latency], ["lower-padding", 820, latency]
  ]) {
    const before = {usage: usage.metrics.rectReads, latency: latency.metrics.rectReads, group: groupReads};
    for (let i = 0; i < 60; i++) {
      group.testHoverListeners.pointermove({clientX: 20 + usage.model.buckets[i % 2].x, clientY: y,
        target: {tagName: "DIV", textContent: region}});
    }
    assert.equal(clock.pending, 1, region);
    assert.equal(usage.metrics.rectReads, before.usage, "physical event handler must not read layout");
    clock.flush();
    assert.equal(usage.metrics.rectReads - before.usage, 1, region);
    assert.equal(latency.metrics.rectReads - before.latency, 1, region);
    assert.equal(groupReads - before.group, 1, region);
    assert.equal(active.tooltip.hidden, false, region);
    assert.equal((active === usage ? latency : usage).tooltip.hidden, true, region);
    assert.match(active.tooltip.innerHTML, /2026-10-14/, region);
    assert.equal(f.element("statsChartsCrosshair").hidden, false, region);
  }
  // The shared group listener stays single even after rebinding the second pane.
  assert.deepEqual(Object.keys(group.testHoverListeners).sort(), ["pointercancel", "pointerleave", "pointermove"]);
  group.testHoverListeners.pointerleave();
  assert.equal(usage.tooltip.hidden, true);
  assert.equal(latency.tooltip.hidden, true);
  audit("Edge path coverage passed", {regions: 8, events_per_region: 60, layout_reads_per_pane_per_frame: 1,
    nearest_pane_tooltip: true, no_interaction_dead_zones: true});
});

test("After leaving the chart, canceling, redrawing, or locking the page, queued hovers never revive", () => {
  for (const cancellable of [true, false]) {
    for (const action of ["leave", "cancel", "render", "lock", "tab"]) {
      const calls = [], f = fixture(async (route, options) => { calls.push({route, options}); return response(204, {}); });
      const clock = animationClock(f, cancellable);
      f.review.state.apiKey = "synthetic-cancel-key";
      f.review.renderStats(statsPayload());
      const groupEvents = {};
      f.element("statsCharts").addEventListener = (event, fn) => { groupEvents[event] = fn; };
      f.element("statsCharts").getBoundingClientRect = () => ({left: 0, top: 0, width: 900, height: 700});
      const usage = hoverChart(f, "usageChart", {left: 0, top: 0, width: 900, height: 280});
      usage.move(0);
      if (action === "leave") groupEvents.pointerleave();
      if (action === "cancel") usage.listeners.pointercancel();
      // Identical polls deliberately no longer redraw; exercise a real update.
      if (action === "render") f.review.renderStats(statsPayload({startedAt: 1760000000001}));
      if (action === "lock") f.review.lockDashboard();
      if (action === "tab") f.review.setTab("accounts");
      clock.flush();
      assert.equal(usage.tooltip.hidden, true, `${action}: stale callback must not restore tooltip`);
      assert.equal(usage.metrics.tooltipWrites, 0);
      assert.equal(f.element("statsChartsCrosshair").hidden, true);
      assert(!calls.some(c => JSON.parse(c.options.body || "{}").event === "stats_chart_hover_started"));
    }
  }
  audit("Queued hover frame cancellation passed", {lifecycle_actions: 5, cancel_api_optional: true, stale_hover_rejected: true});
});

test("When an old frame cannot be physically canceled, only the new chart\u2019s new position is handled after redraw", () => {
  const f = fixture(), clock = animationClock(f, false);
  f.review.renderStats(statsPayload());
  f.element("statsCharts").getBoundingClientRect = () => ({left: 0, top: 0, width: 900, height: 700});
  const oldChart = hoverChart(f, "usageChart", {left: 0, top: 0, width: 900, height: 280});
  oldChart.move(0);
  // Replacing innerHTML removes the old SVG in a browser; mirror that in the
  // stub so the redraw cannot accidentally rebind the detached test element.
  f.element("usageChart").querySelector = () => null;
  f.review.renderStats(statsPayload({startedAt: 1760000000001}));
  const chart = hoverChart(f, "usageChart", {left: 0, top: 0, width: 900, height: 280});
  chart.move(1);
  assert.equal(clock.pending, 2);
  clock.flush();
  assert.equal(oldChart.metrics.tooltipWrites, 0);
  assert.equal(chart.metrics.tooltipWrites, 1);
  assert.match(chart.tooltip.innerHTML, /2026-10-14/);
  assert.equal(chart.tooltip.hidden, false);
  audit("Old frames do not interfere after redraw passed", {generation_guard: true, latest_position_preserved: true});
});

test("Standalone chart hovers also coalesce per frame and clear unpainted events on leave or cancel", () => {
  const f = fixture(), clock = animationClock(f);
  f.element("standaloneChart").innerHTML = f.review.statsComboChart(statsPayload().buckets, {
    lines: [{label: "Token", color: "token", connectZero: true, value: b => b.tokens}]
  });
  const chart = hoverChart(f, "standaloneChart", {left: 0, top: 0, width: 900, height: 280});
  chart.move(0);
  chart.listeners.pointerleave();
  clock.flush();
  assert.equal(chart.tooltip.hidden, true);
  chart.move(1);
  clock.flush();
  assert.equal(chart.tooltip.hidden, false);
  assert.match(chart.tooltip.innerHTML, /2026-10-14/);
  assert.equal(chart.cross.attributes.visibility, "visible");
  chart.move(0);
  chart.listeners.pointercancel();
  clock.flush();
  assert.equal(chart.tooltip.hidden, true);
  assert.equal(chart.cross.attributes.visibility, "hidden");
  audit("Standalone chart per-frame interaction passed", {cancel_on_leave: true, cancel_on_pointercancel: true});
});

test("Bars align with lines: each bar group is centered on its bucket\u2019s line-point time position", () => {
  const f = fixture();
  const svg = f.review.statsComboChart([
    statsBucket("2026-10-13", {tokens: 10, tokenSamples: 1, credits: 10, creditSamples: 1}),
    statsBucket("2026-10-14", {tokens: 20, tokenSamples: 1, credits: 20, creditSamples: 1}),
    statsBucket("2026-10-15", {tokens: 30, tokenSamples: 1, credits: 30, creditSamples: 1}),
  ], {
    lines: [{label: "Total tokens", color: "token", value: b => (b.tokenSamples ? b.tokens : null)}],
    bars: [{label: "Credits", color: "credit", value: b => (b.creditSamples ? b.credits : null)}],
    title: "Aligned", leftFormat: String, box: {w: 600, h: 240}
  });
  const points = [...svg.matchAll(/<circle class="series-token" cx="([\d.]+)"/g)].map(m => Number(m[1]));
  const centres = [...svg.matchAll(/<rect class="chart-bar series-credit" x="([\d.]+)" y="[\d.]+" width="([\d.]+)"/g)]
    .map(m => Number(m[1]) + Number(m[2]) / 2);
  assert.equal(points.length, 3, "Line point count");
  assert.equal(centres.length, 3, "Bar count");
  points.forEach((cx, i) => {
    assert(Math.abs(centres[i] - cx) <= 1.5, "Bar group " + i + " center must match the same-bucket line point: " + centres[i] + " vs " + cx);
  });
  audit("Bar-line alignment passed", {bars_centered_on_points: true});
});

test("Account pool: the add-account form reuses the login flow and each row can delete its credential", async () => {
  const calls = [];
  const f = fixture(async (url, options) => {
    const method = (options && options.method) || "GET";
    calls.push(method + " " + url);
    if (url === "/admin/api/account-add" && method === "GET") return response(200, {defaultFile: "workbuddy4.json"});
    if (url === "/admin/api/account-add" && method === "POST") {
      return response(200, {id: "login-abc", site: "cn", file: "workbuddy4.json", status: "pending",
        authUrl: "https://example.test/auth?state=x", qrPng: "data:image/png;base64,AAAA"});
    }
    if (url.indexOf("/admin/api/account-add?id=") === 0) {
      return response(200, {id: "login-abc", status: "done", savedAs: "workbuddy4.json"});
    }
    if (url === "/admin/api/account-cancel") return response(200, {cancelled: true});
    if (url === "/admin/api/account-delete") return response(200, {deleted: "workbuddy9.json"});
    if (url === "/admin/api/status") return response(200, {gateway: {}, accounts: [], summary: {total: 0}});
    return response(200, {});
  });
  f.review.state.apiKey = "synthetic-admin";
  f.review.state.activeTab = "accounts";

  f.review.renderAccounts([{path: "workbuddy9.json", state: "active", schedulingEnabled: true, edition: "cn"}]);
  const row = f.element("accountsTable").querySelector("tbody").innerHTML;
  assert(row.includes('data-account-delete="workbuddy9.json"'), "Every row must have a delete button carrying the credential name");

  await f.review.openAddAccount();
  assert.equal(f.element("addFile").value, "workbuddy4.json");
  assert(!f.element("addFile").value.includes("/"), "The default name must not contain a path");

  await f.review.startAddAccount();
  assert.equal(f.review.state.addAccountId, "login-abc");
  assert.match(f.element("addAccountQr").src, /^data:image\/png;base64,/);
  assert.match(f.element("addAccountLink").textContent, /example\.test/);

  await f.review.pollAddAccount(true);
  assert.equal(f.review.state.addAccountId, "", "The session must be cleared after completion to stop polling");
  assert.match(f.element("addAccountStatus").textContent, /workbuddy4\.json/);
  assert(calls.some(c => c.indexOf("GET /admin/api/status") === 0), "The account pool should refresh after login success");

  let asked = "";
  const originalConfirm = f.window.confirm;
  f.window.confirm = function (msg) { asked = msg; return true; };
  await f.review.accountRowActions({target: {closest: () => ({getAttribute: () => "workbuddy9.json"})}});
  f.window.confirm = originalConfirm;
  assert.match(asked, /workbuddy9\.json/, "Deletion must confirm and name the credential first");
  assert(calls.indexOf("POST /admin/api/account-delete") > -1, "The delete request must be submitted after confirmation");

  const deletesBefore = calls.filter(c => c === "POST /admin/api/account-delete").length;
  f.window.confirm = function () { return false; };
  await f.review.accountRowActions({target: {closest: () => ({getAttribute: () => "workbuddy9.json"})}});
  f.window.confirm = originalConfirm;
  assert.equal(calls.filter(c => c === "POST /admin/api/account-delete").length, deletesBefore,
    "Canceling confirmation must not submit the deletion");

  const cancelsBefore = calls.filter(c => c === "POST /admin/api/account-cancel").length;
  await f.review.closeAddAccount();
  assert(f.element("addAccountPanel").classList.contains("hidden"), "The form must be hidden after closing");
  assert.equal(calls.filter(c => c === "POST /admin/api/account-cancel").length, cancelsBefore,
    "Closing with no active session must not request cancellation");
  audit("Account add and delete passed", {default_name_no_path: true, confirm_required: true});
});

test("Stats filters are saved to a Cookie: granularity/scope/dates are restored and a stale account falls back to global", async () => {
  const f = fixture(async () => response(200, statsPayload({account: "acc-a"})));
  f.review.state.apiKey = "synthetic-admin";

  // After the user changes granularity, scope, and dates and queries, all four should be written to the Cookie.
  f.element("statsPeriod").value = "week";
  f.element("statsStart").value = "2026-10-01";
  f.element("statsEnd").value = "2026-10-07";
  f.review.state.statsDatesTouched = true;
  f.element("statsAccount").value = "acc-a";
  await f.review.changeStatsFilter(false);
  const saved = JSON.parse(decodeURIComponent(f.cookies.get("gateway_stats_filter")));
  assert.equal(saved.period, "week");
  assert.equal(saved.account, "acc-a");
  assert.equal(saved.start, "2026-10-01");
  assert.equal(saved.end, "2026-10-07");
  const stored = f.cookieWrites[f.cookieWrites.length - 1];
  assert.match(stored, /Path=\/ui/);
  assert.match(stored, /SameSite=Lax/);
  assert.match(stored, /Max-Age=31536000/);

  // A new session opened with the same Cookie restores the controls to the last selection.
  const restored = fixture(async () => response(200, statsPayload({account: "acc-a"})), new Map(f.cookies));
  restored.review.initStatsFilter();
  assert.equal(restored.element("statsPeriod").value, "week");
  assert.equal(restored.element("statsStart").value, "2026-10-01");
  assert.equal(restored.element("statsEnd").value, "2026-10-07");
  assert.equal(restored.review.state.statsAccount, "acc-a");

  // Untouched dates never freeze the default range; the Cookie keeps them empty.
  const plain = fixture(async () => response(200, {}), new Map());
  plain.review.state.apiKey = "synthetic-admin";
  plain.element("statsStart").value = "";
  plain.element("statsEnd").value = "";
  plain.review.state.statsDatesTouched = false;
  await plain.review.changeStatsFilter(false);
  const plainSaved = JSON.parse(decodeURIComponent(plain.cookies.get("gateway_stats_filter")));
  assert.equal(plainSaved.start, "");
  assert.equal(plainSaved.end, "");

  // The remembered account was deleted: after the server returns 400 it is cleared automatically and the query falls back to global.
  const seen = [];
  const staleJar = new Map([["gateway_stats_filter",
    encodeURIComponent(JSON.stringify({period: "day", account: "gone", start: "", end: ""}))]]);
  const stale = fixture(async (url) => {
    seen.push(url);
    return url.indexOf("account=gone") > -1
      ? response(400, {error: "Account stats identifier not found"})
      : response(200, statsPayload());
  }, staleJar);
  stale.review.state.apiKey = "synthetic-admin";
  stale.review.initStatsFilter();
  assert.equal(stale.review.state.statsAccount, "gone");
  await stale.review.loadStats(true);
  assert.equal(stale.review.state.statsAccount, "", "The stale account must be cleared");
  assert(seen.some(function (u) { return u.indexOf("account=") === -1; }), "The query must fall back to global");
  assert.equal(JSON.parse(decodeURIComponent(stale.cookies.get("gateway_stats_filter"))).account, "");

  audit("Stats filter Cookie passed", {restored: true, dates_opt_in: true, stale_account_fallback: true});
});

test("Alert entries render as one row with no log jump; stats scope filters by site; the version chip is clickable", async () => {
  const calls = [];
  const f = fixture(async (url) => {
    calls.push(url);
    if (url.indexOf("/admin/api/stats?") === 0) return response(200, statsPayload());
    return response(200, {});
  });
  f.review.state.apiKey = "synthetic-admin";
  f.review.state.lastStatus = {accounts: [{path: "a.json", state: "cooldown"}]};
  f.review.renderAccountAlerts({alerts: [{account: "a.json", state: "cooldown", severity: "warning",
    label: "Account cooling down", message: "Waiting to recover", at: 1791980005,
    logFile: "gateway-2026-10-16.log", logOffset: 12345}]});
  const alertHtml = f.element("accountAlerts").innerHTML;
  assert(!alertHtml.includes("account-alert-source"), "The log source block must be deleted");
  assert(!alertHtml.includes("data-log-file"), "The log jump entry must be deleted");
  assert(!alertHtml.includes("linklike"), "The jump button style must be deleted");
  assert(/class="account-alert-head">[\s\S]*account-alert-file[\s\S]*<time>/.test(alertHtml),
    "Status, severity, credential, and time must be on the same row");

  f.review.renderStats(statsPayload());
  const options = f.element("statsAccount").innerHTML;
  assert(options.includes('value="site:intl"') && options.includes('value="site:cn"'),
    "The stats scope must include international and domestic sites");
  f.element("statsAccount").value = "site:intl";
  await f.review.changeStatsFilter(false);
  const siteCall = calls.filter(function (u) { return u.indexOf("site=intl") > -1; });
  assert(siteCall.length > 0, "Selecting a site must query by site");
  assert(siteCall.every(function (u) { return u.indexOf("account=") === -1; }), "Site and single-account scopes must never be sent together");

  f.review.renderStatus({gateway: {version: "1.14.17", listen: "127.0.0.1:8317", modelSource: "x"},
    updatedAt: 1791980005, accounts: [], models: []});
  const meta = f.element("meta").innerHTML;
  assert(meta.includes('href="https://github.com/CangShui/workbuddy-gateway"'), "The version must link to the project URL");
  assert(meta.includes('target="_blank"'), "It must open in a new tab");
  assert(meta.includes('rel="noreferrer"'), "It must carry rel=noreferrer");
  audit("Alert row reorder and site filter passed", {jump_removed: true, site_scope: true, chip_link: true});
});
