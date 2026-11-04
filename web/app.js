(function () {
  "use strict";

  var KEY_STORAGE = "wb_gateway_admin_key";
  var POLL_MS = 3000;
  var LIST_FILTERS = [
    {kind: "accounts", id: "onlyUsableAccounts", cookie: "gateway_accounts_only_usable",
      route: "/admin/api/status", statusID: "accountFilterStatus"},
    {kind: "models", id: "onlyUsable", cookie: "gateway_models_only_usable",
      route: "/admin/api/models", statusID: "modelFilterStatus"}
  ];

  var state = {
    apiKey: sessionStorage.getItem(KEY_STORAGE) || "",
    activeTab: "overview",
    lastStatus: null,
    lastModels: null,
    timer: null,
    refreshPending: false,
    alertSource: "",
    alertKind: "",
    alertTimer: null,
    alertVersion: 0,
    settings: null,
    togglePending: false,
    policyVersion: 0,
    logSettings: null,
    logSettingsVersion: 0,
    settingsLoadVersion: 0,
    tasksLastPoll: 0,
    configMode: "preview",
    configPending: false,
    configVersion: 0,
    configPreviewVersion: 0,
    configRevision: "",
    configOriginal: "",
    rateDetails: null,
    rateDetailsAt: 0,
    rateDetailsPending: null,
    openRateKeys: {},
    openRawLogs: {},
    statsPending: false,
    statsVersion: 0,
    statsLastPoll: 0,
    lastStats: null,
    statsRenderKey: "",
    accountAlertsPending: false,
    accountAlertsLastPoll: 0,
    accountAlertsGeneration: 0,
    lastAccountAlerts: null,
    addAccountId: "",
    addAccountPoll: 0,
    addAccountDeadline: 0,
    statsAccount: "",
    statsDatesTouched: false
  };

  function $(id) { return document.getElementById(id); }

  function esc(v) {
    return String(v === undefined || v === null ? "" : v)
      .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;").replace(/'/g, "&#39;");
  }

  function num(v) {
    return (v === undefined || v === null || v === "") ? "-" : v;
  }

  function count(v) { return v === undefined || v === null ? 0 : v; }

  function fmtTime(unix) {
    if (!unix) return "-";
    var d = new Date(unix * 1000);
    if (isNaN(d.getTime())) return "-";
    return d.toLocaleString("en-GB", { hour12: false });
  }

  function fmtMs(v, has) {
    if (!has || v === undefined || v === null) return "-";
    return v < 1000 ? v + "ms" : (v / 1000).toFixed(1) + "s";
  }

  function fmtTokensM(v) {
    if (!v) return "0.00M";
    return (v / 1000000).toFixed(2) + "M";
  }

  function showAlert(msg, ok, source) {
    if (state.alertTimer !== null) clearTimeout(state.alertTimer);
    var version = ++state.alertVersion;
    state.alertSource = source || "";
    state.alertKind = ok ? "success" : "error";
    state.alertTimer = null;
    var el = $("alert");
    el.textContent = msg;
    el.className = "alert" + (ok ? " ok" : "");
    // Successful actions stay readable instead of disappearing on the next poll.
    // Errors remain until their own endpoint recovers or a newer action replaces them.
    if (ok) state.alertTimer = setTimeout(function () {
      if (version === state.alertVersion) hideAlert();
    }, 5000);
  }

  function hideAlert() {
    if (state.alertTimer !== null) clearTimeout(state.alertTimer);
    state.alertTimer = null;
    state.alertVersion++;
    state.alertSource = state.alertKind = "";
    $("alert").className = "alert hidden";
  }

  function setHTMLIfChanged(el, html) {
    if (el._sourceHTML === html && (el.childNodes ? el.childNodes.length : el.innerHTML === html)) return false;
    el.innerHTML = html;
    el._sourceHTML = html;
    return true;
  }

  // Keep stable rows/cells, focus and expanded details during background polling.
  // Only renderers' source markup is compared, not transient detail-panel content.
  function updateTableBody(table, html) {
    if (table._tableSourceHTML === html && (table.children ? table.children.length : table.innerHTML === html)) return;
    if (!document.createElement || !table.children) {
      table.innerHTML = html;
      table._tableSourceHTML = html;
      return;
    }
    var template = document.createElement("template");
    template.innerHTML = html;
    var rows = Array.prototype.slice.call(template.content.children);
    var wanted = Object.create(null), existing = Object.create(null);
    var keyed = rows.every(function (row) {
      var key = row.getAttribute("data-row-key");
      if (key === null || wanted[key]) return false;
      wanted[key] = true;
      return true;
    });
    if (!keyed) {
      table.innerHTML = html;
      table._tableSourceHTML = html;
      return;
    }
    Array.prototype.forEach.call(table.children, function (row) {
      var key = row.getAttribute("data-row-key");
      if (key !== null) existing[key] = row;
    });
    rows.forEach(function (candidate, index) {
      var key = candidate.getAttribute("data-row-key");
      var row = existing[key], source = candidate.outerHTML;
      if (!row) {
        row = candidate;
        Array.prototype.forEach.call(row.cells, function (cell) { cell._sourceHTML = cell.outerHTML; });
      } else if (row._sourceHTML !== source) {
        if (row.cells.length !== candidate.cells.length) {
          row.replaceWith(candidate);
          row = candidate;
          Array.prototype.forEach.call(row.cells, function (cell) { cell._sourceHTML = cell.outerHTML; });
        } else {
          Array.prototype.slice.call(candidate.cells).forEach(function (cell, i) {
            if (row.cells[i]._sourceHTML !== cell.outerHTML) {
              cell._sourceHTML = cell.outerHTML;
              row.replaceChild(cell, row.cells[i]);
            }
          });
        }
      }
      row._sourceHTML = source;
      var before = table.children[index];
      if (before !== row) table.insertBefore(row, before || null);
    });
    Array.prototype.slice.call(table.children).forEach(function (row) {
      var key = row.getAttribute("data-row-key");
      if (key === null || !wanted[key]) row.remove();
    });
    table._tableSourceHTML = html;
  }

  // setConn updates the persistent connection status in the top-right corner, so every request's outcome stays visible (no flash).
  function setConn(text, cls) {
    var el = $("conn");
    if (!el) return;
    el.textContent = text;
    el.className = "conn" + (cls ? " " + cls : "");
  }

  function nowText() {
    return new Date().toLocaleTimeString("en-GB", { hour12: false });
  }

  function traceID() {
    return "web-" + Date.now().toString(36) + "-" + Math.random().toString(36).slice(2);
  }

  function reportUIEvent(event, path, status, trace, enabled) {
    if (!state.apiKey || path === "/admin/api/setup") return;
    var record = {event: event, route: path.split("?")[0], status: status};
    if (typeof enabled === "boolean") record.enabled = enabled;
    fetch("/admin/api/client-events", {
      method: "POST",
      headers: {"Authorization": "Bearer " + state.apiKey, "Content-Type": "application/json",
        "X-Workbuddy-Admin": "1", "X-Trace-ID": trace},
      body: JSON.stringify(record)
    }).catch(function () { /* The original failure is still audited server-side; do not recurse on failed event reports. */ });
  }

  function lockDashboard() {
    resetConfigEditor();
    state.tasksLastPoll = 0;
    $("tabs").classList.add("hidden");
    $("dashboard").classList.add("hidden");
    state.lastStatus = null;
    state.lastModels = null;
    state.rateDetails = null;
    state.rateDetailsAt = 0;
    state.rateDetailsPending = null;
    state.openRateKeys = {};
    state.openRawLogs = {};
    state.statsVersion++;
    state.statsLastPoll = 0;
    state.lastStats = null;
    state.statsRenderKey = "";
    hideAllStatsChartHover();
    statsChartControllers = [];
    ["usageSummary", "usageChart", "latencyChart", "statsInfo", "statsRange"].forEach(function (id) {
      $(id).textContent = "";
      $(id)._sourceHTML = "";
    });
    if ($("latencyLegend")) {
      $("latencyLegend").textContent = "";
      $("latencyLegend")._sourceHTML = "";
    }
    $("statsAccount").innerHTML = '<option value="">Global · All accounts</option>';
    $("statsAccount")._sourceHTML = "";
    $("statsTable").querySelector("tbody").innerHTML = "";
    $("statsTable").querySelector("tbody")._sourceHTML = "";
    state.accountAlertsGeneration++;
    state.accountAlertsLastPoll = 0;
    state.lastAccountAlerts = null;
    state.addAccountId = "";
    state.addAccountPoll = 0;
    state.addAccountDeadline = 0;
    $("poolHealth").textContent = "—";
    $("poolHealth").className = "health-value";
    $("poolHealthBar").style.width = "0%";
    $("poolHealthStatus").textContent = "Waiting for account status";
    $("accountAlertsInfo").textContent = "Account anomaly logs load after login.";
    $("accountAlerts").innerHTML = '<p class="stats-empty">Waiting for logs</p>';
    $("accountAlerts")._alertSourceHTML = null;
    if ($("accountAlertTooltip")) $("accountAlertTooltip").hidden = true;
    ["meta", "gatewayInfo", "logBox", "configBox", "tasksInfo"].forEach(function (id) {
      $(id).textContent = "";
      $(id)._sourceHTML = "";
    });
    ["accountsTable", "modelsTable", "tasksTable"].forEach(function (id) {
      var body = $(id).querySelector("tbody");
      body.innerHTML = "";
      body._tableSourceHTML = "";
    });
  }

  function api(path, options) {
    options = options || {};
    var trace = options.trace || traceID();
    var keyAtStart = state.apiKey;
    var headers = {"X-Trace-ID": trace};
    if (state.apiKey) headers["Authorization"] = "Bearer " + state.apiKey;
    if (options.method === "POST") {
      headers["Content-Type"] = "application/json";
      headers["X-Workbuddy-Admin"] = "1";
    }
    var status = 0;
    return fetch(path, {headers: headers, method: options.method || "GET", cache: "no-store",
      body: options.body === undefined ? undefined : JSON.stringify(options.body)}).then(function (resp) {
      status = resp.status;
      return resp.json().then(function (data) { return {resp: resp, data: data}; });
    }).then(function (result) {
      if (keyAtStart !== state.apiKey) throw new Error("Login state changed; ignoring stale request result");
      var resp = result.resp;
      if (resp.status === 401) {
        state.apiKey = "";
        sessionStorage.removeItem(KEY_STORAGE);
        lockDashboard();
        setConn("Auth failed (401)", "danger");
        showAlert("Admin auth failed: enter the gateway.adminKey from config.json; the model API Key cannot be used for admin login.");
        throw new Error("Admin auth failed");
      }
      if (!resp.ok) {
        if (!options.quiet) {
          setConn("Request failed (" + resp.status + ")", "danger");
          showAlert("HTTP " + resp.status + ": " + (result.data.error || "Request failed") +
            (path.split("?")[0] === "/admin/api/config-edit" ? "; operation not completed, your edit input is preserved." : "; previous data preserved, nothing was zeroed out."),
            false, path.split("?")[0]);
        }
        var failure = new Error("HTTP " + resp.status);
        failure.status = resp.status;
        failure.detail = result.data.error || "Request failed";
        throw failure;
      }
      if (state.alertKind === "error" && state.alertSource === path.split("?")[0]) hideAlert();
      setConn("Connected · " + nowText(), "ok");
      reportUIEvent("request_completed", path, status, trace);
      return result.data;
    }).catch(function (err) {
      if (keyAtStart === state.apiKey) {
        reportUIEvent("request_failed", path, status, trace);
        if (!status && !options.quiet) {
          setConn("Connection failed", "danger");
          showAlert("Request did not complete; previous data preserved. Check your network and retry.", false, path.split("?")[0]);
        }
      }
      throw err;
    });
  }

  // ---- Rendering: overview --------------------------------------------------------
  function renderPoolHealth(data) {
    var h = data.health || {}, p = h.percent;
    var known = !data.stale && typeof p === "number" && isFinite(p) && p >= 0 && p <= 100;
    var stateClass = known ? (p === 100 ? "healthy" : (p === 0 ? "unavailable" : "degraded")) : "unknown";
    $("poolHealth").textContent = known ? p.toLocaleString("en-GB", {maximumFractionDigits: 1}) + "%" : "—";
    $("poolHealth").className = "health-value " + stateClass;
    $("poolHealthBar").style.width = known ? p + "%" : "0%";
    $("poolHealthBar").className = stateClass;
    $("poolHealthStatus").textContent = data.stale ? "Snapshot too old · health pending confirmation" :
      h.state === "empty" ? "No accounts loaded · no health data" : !known ? "Health pending confirmation" :
      p === 100 ? "All accounts in the pool are usable" : p === 0 ? "Account pool currently unavailable; see alerts" : "Account pool partially limited; see alerts";
  }

  function renderAccountAlerts(data) {
    state.lastAccountAlerts = data;
    var labels = {urgent: "Urgent", important: "Important", warning: "Warning", notice: "Notice"};
    var alerts = data.alerts || [];
    var snapshot = state.lastStatus;
    // During the short log-scan cache, keep using the latest account snapshot to drop recovered items instead of treating past failures as current alerts.
    if (snapshot && !snapshot.stale && Array.isArray(snapshot.accounts)) {
      alerts = alerts.filter(function (a) {
        return snapshot.accounts.some(function (acc) {
          return acc.path === a.account && (a.state === "paused" ? acc.schedulingEnabled === false :
            a.state === (acc.state === "quota_exhausted" ? "paid_exhausted" : acc.state));
        });
      });
    }
    var notes = [];
    if (data.stale || (snapshot && snapshot.stale)) notes.push("Snapshot too old; showing the last confirmed anomalies");
    if (data.missingLogCount) notes.push("Some anomalies have no matching log yet; they may appear after the next scan");
    if (data.scanTruncated || data.listTruncated) notes.push("Read limit reached; showing only records within the scanned range");
    $("accountAlertsInfo").textContent = notes.length ? notes.join("; ") + "." : "";
    var html = alerts.length ? alerts.map(function (a) {
      var level = labels[a.severity] ? a.severity : "notice";
      var detail = labels[level] + " · " + a.label + " · " + a.account +
        (a.at ? " · " + fmtTime(a.at) : "") + (a.message ? "\n" + a.message : "");
      return '<article class="account-alert alert-' + level + '" tabindex="0" data-alert-detail="' +
        esc(detail) + '" aria-label="' + esc(detail) + '"><div class="account-alert-head">' +
        '<span class="alert-priority">' + esc(labels[level]) + '</span><strong>' + esc(a.label) +
        '</strong><span class="account-alert-file">' + esc(a.account) + '</span><time>' + esc(fmtTime(a.at)) +
        '</time></div></article>';
    }).join("") : '<p class="stats-empty">' + (data.missingLogCount ? "Anomalies exist, but no matching logs were found within the scanned range" :
      data.stale ? "No log records yet; account status pending confirmation" : "No current account anomaly logs found") + '</p>';
    // Unchanged polling must not replace a hovered/focused row or dismiss its details.
    if ($("accountAlerts")._alertSourceHTML !== html) {
      if ($("accountAlertTooltip")) $("accountAlertTooltip").hidden = true;
      $("accountAlerts").innerHTML = html;
      $("accountAlerts")._alertSourceHTML = html;
    }
  }

  function bindAccountAlertTooltip() {
    var list = $("accountAlerts"), tip = $("accountAlertTooltip");
    if (!list || !tip || !list.addEventListener) return;
    var pinned = null;
    var hide = function () {
      pinned = null;
      tip.hidden = true;
    };
    var show = function (target, x, y) {
      if (!target) return;
      var detail = target.getAttribute("data-alert-detail") || "";
      if (!detail) return;
      tip.textContent = detail;
      tip.hidden = false;
      var width = tip.offsetWidth || 260, height = tip.offsetHeight || 80;
      var left = Math.max(8, Math.min(x + 14, window.innerWidth - width - 8));
      var top = y + 14;
      if (top + height > window.innerHeight - 8) top = y - height - 14;
      tip.style.left = left + "px";
      tip.style.top = Math.max(8, top) + "px";
    };
    list.addEventListener("pointermove", function (event) {
      if (event.pointerType === "touch") return;
      var target = event.target && event.target.closest ? event.target.closest(".account-alert") : null;
      if (!target) { hide(); return; }
      show(target, event.clientX, event.clientY);
    });
    // Touch emits pointerleave after every tap; it is not a hover exit.
    list.addEventListener("pointerleave", function (event) {
      if (event.pointerType !== "touch") hide();
    });
    list.addEventListener("pointercancel", hide);
    list.addEventListener("scroll", hide, {passive: true});
    list.addEventListener("focusin", function (event) {
      var target = event.target && event.target.closest ? event.target.closest(".account-alert") : null;
      if (!target) return;
      var rect = target.getBoundingClientRect ? target.getBoundingClientRect() : {left: 0, bottom: 0};
      show(target, rect.left, rect.bottom);
    });
    list.addEventListener("focusout", hide);
    list.addEventListener("click", function (event) {
      var target = event.target && event.target.closest ? event.target.closest(".account-alert") : null;
      if (!target) return;
      var rect = target.getBoundingClientRect ? target.getBoundingClientRect() : {left: 0, bottom: 0};
      // Focus/hover can already have shown the tooltip before click fires.
      // Track explicit clicks separately, so a first tap never immediately hides it.
      if (pinned === target && !tip.hidden) hide();
      else { pinned = target; show(target, rect.left, rect.bottom); }
    });
    window.addEventListener("blur", hide);
  }

  function loadAccountAlerts() {
    if (!state.apiKey || state.accountAlertsPending || Date.now() - state.accountAlertsLastPoll < 10000) return Promise.resolve();
    var generation = state.accountAlertsGeneration, key = state.apiKey;
    state.accountAlertsPending = true;
    state.accountAlertsLastPoll = Date.now();
    return api("/admin/api/account-alerts").then(function (data) {
      if (generation === state.accountAlertsGeneration && key === state.apiKey) renderAccountAlerts(data);
    }).catch(function () {
      if (generation === state.accountAlertsGeneration && key === state.apiKey) {
        $("accountAlertsInfo").textContent = "Failed to read alert logs; keeping the previous result. This does not mean the account pool has no anomalies.";
      }
    }).then(function () { state.accountAlertsPending = false; });
  }

  // Stats facts come from the optional stats.json; all queries use admin auth and the existing front-end persisted audit.
  function statsNumber(v) {
    return Number(v || 0).toLocaleString("en-GB", {maximumFractionDigits: 2});
  }

  function statsCredit(v) {
    return Number(v || 0).toLocaleString("en-GB", {maximumFractionDigits: 4});
  }

  function statsDuration(v) {
    return v === null || v === undefined ? "—" : (Number(v) / 1000).toFixed(2) + " s";
  }

  function statsCard(label, value, detail, color) {
    return '<article class="usage-metric ' + color + '"><div class="usage-label">' + esc(label) +
      '</div><div class="usage-value">' + esc(value) + '</div><div class="usage-detail">' + esc(detail) + '</div></article>';
  }

  function statsChartNow(value) {
    return typeof value === "number" && isFinite(value) && value > 0 ? value : Date.now();
  }

  function statsBucketIsFuture(bucket, now) {
    // Bucket start is an absolute Unix millisecond timestamp, not the displayed
    // MM-DD label. This works across years and browser time zones.
    return typeof bucket.start === "number" && isFinite(bucket.start) && bucket.start > now;
  }

  function statsChartValue(series, bucket, index, now) {
    if (statsBucketIsFuture(bucket, statsChartNow(now))) return null;
    var value = series.value(bucket, index);
    return value === null || value === undefined || !isFinite(value) ? null : Number(value);
  }

  function statsChartAxes(buckets, opts, cutoff) {
    var now = statsChartNow(cutoff === undefined ? opts.nowMs : cutoff);
    var lines = [], bars = [];
    (opts.lines || []).forEach(function (series) { buckets.forEach(function (b, i) {
      if (statsBucketIsFuture(b, now)) return;
      var v = statsChartValue(series, b, i, now);
      if (v !== null || series.connectZero) lines.push(v === null ? 0 : v);
    }); });
    (opts.bars || []).forEach(function (series) { buckets.forEach(function (b, i) {
      var v = statsChartValue(series, b, i, now);
      if (v !== null) bars.push(v);
    }); });
    var dual = !!opts.rightFormat && bars.length > 0;
    var scaleMax = function (values) {
      var maximum = 0;
      values.forEach(function (v) { maximum = Math.max(maximum, v); });
      return (maximum > 0 ? maximum : 1) * 1.12;
    };
    var leftMax = scaleMax(dual ? lines : lines.concat(bars)), rightMax = scaleMax(dual ? bars : []);
    var ticks = function (format, maximum) {
      return [0, 1, 2, 3, 4].map(function (n) { return String(format(maximum * (1 - n / 4))); });
    };
    var leftTicks = ticks(opts.leftFormat || String, leftMax);
    var rightTicks = dual ? ticks(opts.rightFormat, rightMax) : [];
    var padding = function (labels, minimum) {
      return Math.min(130, Math.max(minimum, 14 + Math.max.apply(null, labels.map(function (t) { return t.length; })) * 7));
    };
    return {hasValues: lines.length + bars.length > 0, dual: dual,
      leftMax: leftMax, rightMax: rightMax, leftTicks: leftTicks, rightTicks: rightTicks,
      left: padding(leftTicks, 52), right: dual ? padding(rightTicks, 44) : 16};
  }

  // Native SVG, no CDN access. Lines by default bridge empty buckets and connect adjacent samples;
  // with series.connectZero=true, empty buckets are drawn as 0 (used by the usage chart).
  // Bars still only appear in buckets with samples; opts = {lines, bars, title, leftFormat, rightFormat, box}.
  function statsComboChart(buckets, opts) {
    opts = opts || {};
    var lines = opts.lines || [], bars = opts.bars || [];
    var title = opts.title || "";
    var leftFormat = opts.leftFormat || function (v) { return String(v); };
    var rightFormat = opts.rightFormat || null;
    var box = opts.box || {w: 900, h: 260};
    var now = statsChartNow(opts.nowMs);
    var pick = function (s, b, i) { return statsChartValue(s, b, i, now); };
    var lineValue = function (s, b, i) {
      if (statsBucketIsFuture(b, now)) return null;
      var value = pick(s, b, i);
      return value === null && s.connectZero ? 0 : value;
    };
    var axes = statsChartAxes(buckets, opts, now);
    if (!axes.hasValues) {
      var futureOnly = buckets.length && buckets.every(function (b) { return statsBucketIsFuture(b, now); });
      return '<p class="stats-empty">' + (futureOnly ? "The selected time range has not started yet" : "No samples available for this time range") + '</p>';
    }
    var dual = axes.dual, leftMax = axes.leftMax, rightMax = axes.rightMax;
    var width = Math.max(260, Math.round(box.w)), height = Math.max(190, Math.round(box.h));
    var top = 18, bottom = 40;
    var leftTicks = axes.leftTicks, rightTicks = axes.rightTicks;
    var left = axes.left, right = axes.right;
    // Date ticks and the crosshair align pixel-perfectly only when both charts share the same margins.
    if (opts.plotPad) {
      if (opts.plotPad.left != null) left = opts.plotPad.left;
      if (opts.plotPad.right != null) right = opts.plotPad.right;
    }
    var plotW = width - left - right, plotH = height - top - bottom;
    var x = function (i) { return buckets.length < 2 ? left + plotW / 2 : left + i * plotW / (buckets.length - 1); };
    var yLeft = function (v) { return top + plotH * (1 - v / leftMax); };
    var yRight = function (v) { return top + plotH * (1 - v / rightMax); };
    var out = '<div class="chart-canvas"><svg viewBox="0 0 ' + width + ' ' + height + '" role="img" aria-label="' + esc(title) +
      '" data-chart-model=""><title>' + esc(title) + '</title>';
    for (var n = 0; n <= 4; n++) {
      var yy = top + plotH * n / 4;
      out += '<line class="chart-grid" x1="' + left + '" y1="' + yy + '" x2="' + (width - right) + '" y2="' + yy + '"/>' +
        '<text class="chart-label" text-anchor="end" x="' + (left - 8) + '" y="' + (yy + 4) + '">' + esc(leftTicks[n]) + '</text>';
      if (dual) {
        out += '<text class="chart-label" text-anchor="start" x="' + (width - right + 8) + '" y="' + (yy + 4) + '">' + esc(rightTicks[n]) + '</text>';
      }
    }
    // On narrow screens dates shrink to MM-DD; tick count is derived from the available width to avoid overlapping labels.
    var narrow = width < 520;
    var tickLabel = function (text) {
      return narrow && /^\d{4}-\d{2}-\d{2}$/.test(text) ? text.slice(5) : text;
    };
    var widest = 0;
    buckets.forEach(function (b) { widest = Math.max(widest, tickLabel(b.label).length); });
    var perTick = Math.max(44, widest * 7 + 14);
    var maxTicks = Math.max(2, Math.floor(plotW / perTick));
    var every = Math.max(1, Math.ceil(buckets.length / maxTicks));
    // First/last labels hug the edges (centering would overflow and clip the viewBox), then a greedy pixel-spacing pass:
    // the last tick has top priority and evicts its predecessor on conflict, so the final date is always visible.
    var labelW = widest * 6.6;
    var lastIdx = buckets.length - 1;
    var candidates = [];
    for (var ci = 0; ci < buckets.length; ci += every) {
      candidates.push(ci);
    }
    if (candidates[candidates.length - 1] !== lastIdx) {
      candidates.push(lastIdx);
    }
    var geometry = function (i, isLast) {
      var anchor = i === 0 ? "start" : (isLast ? "end" : "middle");
      var tx = i === 0 ? left : (isLast ? width - right : x(i));
      var l = anchor === "start" ? tx : (anchor === "end" ? tx - labelW : tx - labelW / 2);
      return {i: i, anchor: anchor, tx: tx, left: l, right: l + labelW};
    };
    var kept = [];
    candidates.forEach(function (i) {
      var isLast = i === lastIdx, geo = geometry(i, isLast);
      if (isLast) {
        while (kept.length && geo.left - kept[kept.length - 1].right < 8) {
          kept.pop();
        }
      } else if (kept.length && geo.left - kept[kept.length - 1].right < 8) {
        return;
      }
      kept.push(geo);
    });
    kept.forEach(function (t) {
      out += '<text class="chart-label" text-anchor="' + t.anchor + '" x="' + t.tx + '" y="' + (height - 12) + '">' +
        esc(tickLabel(buckets[t.i].label)) + '</text>';
    });
    var model = {width: width, height: height, left: left, top: top, plotW: plotW, plotH: plotH, count: buckets.length, buckets: []};
    if (bars.length) {
      // Bar groups must be centered on the time position of that bucket's line point, otherwise bars drift half a bucket;
      // this is only SVG boundary protection and never triggers at normal sizes.
      var slot = plotW / Math.max(1, buckets.length);
      var groupW = Math.min(slot * 0.7, 24);
      var barW = Math.max(1, groupW / bars.length);
      var baseY = top + plotH;
      buckets.forEach(function (b, i) {
        var groupX = Math.min(Math.max(x(i) - groupW / 2, 0), width - groupW);
        bars.forEach(function (s, j) {
          var value = pick(s, b, i);
          if (value === null) return;
          var yy = (dual ? yRight : yLeft)(value);
          var h = Math.max(1, baseY - yy);
          out += '<rect class="chart-bar series-' + s.color + '" x="' + (groupX + j * barW).toFixed(2) +
            '" y="' + yy.toFixed(2) + '" width="' + Math.max(1, barW - 1).toFixed(2) + '" height="' + h.toFixed(2) + '"></rect>';
        });
      });
    }
    lines.forEach(function (s) {
      var points = [], paths = [], connectGaps = s.connectGaps !== false;
      buckets.forEach(function (b, i) {
        var value = lineValue(s, b, i);
        if (value === null) {
          if (!connectGaps && points.length) {
            paths.push(points.join(" "));
            points = [];
          }
          return;
        }
        var xx = x(i), yy = yLeft(value);
        points.push((points.length ? "L" : "M") + xx.toFixed(2) + "," + yy.toFixed(2));
        out += '<circle class="series-' + s.color + '" cx="' + xx.toFixed(2) + '" cy="' + yy.toFixed(2) +
          '" r="' + (buckets.length > 80 ? 1.8 : 3.2) + '"></circle>';
      });
      if (points.length) paths.push(points.join(" "));
      paths.forEach(function (p) { out += '<path class="chart-line series-' + s.color + '" d="' + p + '"/>'; });
    });
    buckets.forEach(function (b, i) {
      var row = {label: b.label, x: x(i), future: statsBucketIsFuture(b, now), lines: [], bars: []};
      lines.forEach(function (s) {
        var value = lineValue(s, b, i);
        row.lines.push({label: s.label, color: s.color, value: value,
          display: value === null ? "—" : leftFormat(value), y: value === null ? null : yLeft(value)});
      });
      bars.forEach(function (s) {
        var value = pick(s, b, i);
        row.bars.push({label: s.label, color: s.color, value: value,
          display: value === null ? "—" : (dual ? rightFormat : leftFormat)(value),
          y: value === null ? null : (dual ? yRight : yLeft)(value)});
      });
      // Shared overview readout is separate from the model-specific plotted series.
      row.readout = typeof opts.tooltipRows === "function" ? opts.tooltipRows(b) : null;
      if (row.future && row.readout) row.readout = row.readout.map(function (item) {
        return {label: item.label, color: item.color, display: "—"};
      });
      model.buckets.push(row);
    });
    // The crosshair, hit dots, and tooltip are all driven by the same geometry model to avoid drift when the SVG scales.
    out += '<line class="chart-crosshair" x1="0" y1="0" x2="0" y2="0" visibility="hidden"/>' +
      '<g class="chart-hover-dots">';
    lines.forEach(function (s) {
      out += '<circle class="chart-hover-dot series-' + s.color + '" r="4" visibility="hidden"/>';
    });
    out += '</g><rect class="chart-hover-area" x="' + left + '" y="' + top + '" width="' + plotW + '" height="' + plotH +
      '" fill="transparent" pointer-events="all"/>';
    out += '</svg><div class="chart-tooltip" hidden></div></div>';
    return out.replace('data-chart-model=""', 'data-chart-model="' + esc(JSON.stringify(model)) + '"');
  }

  function bindStatsChartHover(container) {
    if (!container || !container.querySelector) return;
    var svg = container.querySelector("svg");
    var tooltip = container.querySelector(".chart-tooltip");
    if (!svg || !tooltip || !svg.getAttribute) return;
    var model;
    try { model = JSON.parse(svg.getAttribute("data-chart-model") || "{}"); } catch (_) { return; }
    if (!model.buckets || !model.buckets.length) return;
    var area = svg.querySelector(".chart-hover-area");
    var cross = svg.querySelector(".chart-crosshair");
    var dots = svg.querySelectorAll ? svg.querySelectorAll(".chart-hover-dot") : [];
    if (!area || !cross) return;
    var group = (container.id === "usageChart" || container.id === "latencyChart") ? $("statsCharts") : null;
    var sharedLine = group ? $("statsChartsCrosshair") : null;
    var plottedIndex = -1, tooltipIndex = -1, tooltipSize = null, tooltipHTML = [];
    if (sharedLine) cross.setAttribute("visibility", "hidden");
    var hide = function () {
      cancelStatsChartHover(controller);
      cross.setAttribute("visibility", "hidden");
      Array.prototype.forEach.call(dots, function (dot) { dot.setAttribute("visibility", "hidden"); });
      tooltip.hidden = true;
      plottedIndex = -1;
    };
    var show = function (index, withTooltip, pointer, rect) {
      var row = model.buckets[index];
      if (!row) return;
      // Only the active pane needs a tooltip or a size measurement. Build its
      // content once per bucket, not once per physical pointer event.
      if (withTooltip !== false) {
        if (tooltipIndex !== index) {
          if (!tooltipHTML[index]) {
            var rows = row.readout || row.lines.concat(row.bars);
            tooltipHTML[index] = '<div class="chart-tooltip-date">' + esc(row.label) + '</div>' +
              (row.future ? '<div class="chart-tooltip-status">Not yet due</div>' : '') +
              rows.map(function (item) {
                var dot = item.color ? '<i class="series-dot ' + esc(item.color) + '"></i>' : "";
                return '<div class="chart-tooltip-row"><span class="chart-tooltip-key">' + dot + esc(item.label) +
                  '</span><span class="chart-tooltip-value">' + esc(item.display) + '</span></div>';
              }).join("");
          }
          tooltip.innerHTML = tooltipHTML[index];
          tooltipIndex = index;
          tooltipSize = null;
        }
        if (tooltip.hidden) tooltip.hidden = false;
        // One layout read when content changes. All chart/group rectangles
        // have already been read before this frame starts changing the DOM.
        if (!tooltipSize) tooltipSize = {width: tooltip.offsetWidth || 180, height: tooltip.offsetHeight || 80};
      } else if (!tooltip.hidden) {
        tooltip.hidden = true;
      }
      if (plottedIndex !== index) {
        if (!sharedLine) {
          cross.setAttribute("x1", row.x); cross.setAttribute("x2", row.x);
          cross.setAttribute("y1", model.top); cross.setAttribute("y2", model.top + model.plotH);
          cross.setAttribute("visibility", "visible");
        }
        row.lines.forEach(function (line, i) {
          var dot = dots[i];
          if (!dot) return;
          if (line.value === null) { dot.setAttribute("visibility", "hidden"); return; }
          dot.setAttribute("cx", row.x); dot.setAttribute("cy", line.y);
          dot.setAttribute("visibility", "visible");
        });
        plottedIndex = index;
      }
      if (withTooltip === false) return;
      var scaleX = rect.width ? rect.width / model.width : 1;
      var scaleY = rect.height ? rect.height / model.height : 1;
      var px = pointer && isFinite(pointer.clientX) ? pointer.clientX - rect.left : row.x * scaleX;
      var py = pointer && isFinite(pointer.clientY) ? pointer.clientY - rect.top : (model.top + model.plotH / 2) * scaleY;
      var width = tooltipSize.width, height = tooltipSize.height;
      var left = px + 14;
      if (left + width > rect.width) left = px - width - 14;
      if (left < 4) left = 4;
      var top = py - height / 2;
      if (top < 4) top = 4;
      if (top + height > rect.height) top = rect.height - height - 4;
      tooltip.style.transform = "translate3d(" + left + "px," + Math.max(4, top) + "px,0)";
    };
    if (!group || !sharedLine) {
      area.addEventListener("pointermove", function (event) {
        queueStatsChartHover(controller, event);
      });
    }
    area.addEventListener("pointerleave", function () {
      // Keep the guide through the inter-card gap; the group owns its lifetime.
      if (!group || !sharedLine) hide();
    });
    area.addEventListener("pointercancel", group && sharedLine ? hideAllStatsChartHover : hide);
    var controller = {
      id: container.id, show: show, hide: hide,
      group: group, guide: sharedLine, model: model,
      measure: function () {
        return svg.getBoundingClientRect ? svg.getBoundingClientRect() :
          {left: 0, top: 0, width: model.width, height: model.height};
      },
      position: function (index, rect) {
        var row = model.buckets[index];
        if (!row) return null;
        var scale = rect.height / model.height;
        return {x: rect.left + row.x * rect.width / model.width,
          top: rect.top + model.top * scale, bottom: rect.top + (model.top + model.plotH) * scale};
      }
    };
    if (group && sharedLine) {
      statsChartControllers.push(controller);
      if (!group.statsHoverBound) {
        // The axes, labels, card padding and inter-card gap are part of the
        // linked chart too. Listening only on the transparent plot rect leaves
        // dead zones where the retained guide appears to freeze.
        group.addEventListener("pointermove", function (event) {
          queueStatsChartHover(null, event, group);
        });
        group.addEventListener("pointerleave", hideAllStatsChartHover);
        group.addEventListener("pointercancel", hideAllStatsChartHover);
        group.statsHoverBound = true;
      }
    }
  }

  var statsChartControllers = [];
  var statsChartHoverTrace = "";
  var statsChartHoverFrame = null, statsChartHoverPending = null, statsChartHoverGeneration = 0;

  function cancelStatsChartHover(controller) {
    if (controller && (!statsChartHoverPending || statsChartHoverPending.controller !== controller)) return;
    if (statsChartHoverFrame !== null && window.cancelAnimationFrame) window.cancelAnimationFrame(statsChartHoverFrame);
    statsChartHoverFrame = null;
    statsChartHoverPending = null;
    statsChartHoverGeneration++;
  }

  function queueStatsChartHover(controller, event, group) {
    // Keep only the latest coordinates across BOTH charts. High-polling-rate
    // mice must not generate hundreds of DOM/layout updates between frames.
    if (group && !statsChartControllers.length) return;
    statsChartHoverPending = {controller: controller, group: group, clientX: event.clientX, clientY: event.clientY};
    if (statsChartHoverFrame !== null) return;
    var generation = statsChartHoverGeneration;
    var paint = function () {
      if (generation !== statsChartHoverGeneration) return;
      statsChartHoverFrame = null;
      var pointer = statsChartHoverPending;
      statsChartHoverPending = null;
      if (!pointer) return;
      var source = pointer.controller, rect, frames = null;
      if (pointer.group) {
        // Pick the nearest SVG, including its axes, without reading layout in
        // the physical pointer handler. A gap/title hover uses the nearer pane.
        var nearest = Infinity;
        frames = statsChartControllers.filter(function (c) { return c.group === pointer.group; }).map(function (c) {
          var bounds = c.measure();
          var distance = Math.max(bounds.top - pointer.clientY, pointer.clientY - bounds.top - bounds.height, 0);
          if (!source || distance < nearest) {
            source = c;
            rect = bounds;
            nearest = distance;
          }
          return {controller: c, rect: bounds};
        });
        if (!source) return;
      } else {
        rect = source.measure();
      }
      var model = source.model;
      var viewX = (pointer.clientX - rect.left) * model.width / (rect.width || model.width);
      var index = Math.round((viewX - model.left) / Math.max(1, model.plotW) * (model.count - 1));
      index = Math.max(0, Math.min(model.count - 1, index));
      if (source.group && source.guide) {
        syncStatsChartHover(index, source, source.guide, source.group, pointer, rect, frames);
      } else {
        source.show(index, true, pointer, rect);
      }
    };
    if (window.requestAnimationFrame) statsChartHoverFrame = window.requestAnimationFrame(paint);
    else paint();
  }

  function syncStatsChartHover(index, source, guide, group, pointer, sourceRect, measuredFrames) {
    // Read every rectangle before either pane mutates the DOM, avoiding
    // alternating read/write forced layouts in the linked chart.
    var positions = [], frames = measuredFrames || statsChartControllers.filter(function (c) {
      return c.group === group;
    }).map(function (controller) {
      return {controller: controller, rect: controller === source ? sourceRect : controller.measure()};
    });
    frames.forEach(function (frame) {
      var position = frame.controller.position(index, frame.rect);
      if (position) positions.push(position);
    });
    if (!positions.length) return;
    var bounds = group.getBoundingClientRect();
    var top = Math.min.apply(null, positions.map(function (p) { return p.top; }));
    var bottom = Math.max.apply(null, positions.map(function (p) { return p.bottom; }));
    source.show(index, true, pointer, sourceRect);
    frames.forEach(function (frame) {
      if (frame.controller !== source) frame.controller.show(index, false, null, frame.rect);
    });
    guide.style.transform = "translate3d(" + (positions[0].x - bounds.left) + "px," + (top - bounds.top) + "px,0)";
    var height = (bottom - top) + "px";
    if (guide.style.height !== height) guide.style.height = height;
    guide.hidden = false;
    if (!statsChartHoverTrace) {
      statsChartHoverTrace = traceID();
      reportUIEvent("stats_chart_hover_started", "/admin/api/stats", 0, statsChartHoverTrace);
    }
  }

  function hideAllStatsChartHover() {
    cancelStatsChartHover();
    statsChartControllers.forEach(function (controller) { controller.hide(); });
    var guide = $("statsChartsCrosshair");
    if (guide) guide.hidden = true;
    if (statsChartHoverTrace) {
      reportUIEvent("stats_chart_hover_ended", "/admin/api/stats", 0, statsChartHoverTrace);
      statsChartHoverTrace = "";
    }
  }

  function statsLineChart(buckets, series, title, format, box) {
    return statsComboChart(buckets, {lines: series, bars: [], title: title, leftFormat: format, box: box});
  }

  function renderStatsChart(id, buckets, opts) {
    var el = $(id);
    if (!el) return;
    el.innerHTML = statsComboChart(buckets, opts);
    bindStatsChartHover(el);
  }

  function renderStats(data) {
    state.lastStats = data;
    var s = data.summary || {}, rows = data.buckets || [], store = data.storage || {};
    var chartBox = function (id) {
      var el = $(id), w = el && el.clientWidth ? el.clientWidth : 900;
      return {w: w, h: w < 520 ? 230 : 280};
    };
    var chartWidth = Math.min(chartBox("usageChart").w, chartBox("latencyChart").w);
    var chartNow = statsChartNow(data.serverTime);
    var payload = {};
    Object.keys(data).forEach(function (key) {
      if (key !== "serverTime" && key !== "storage") payload[key] = data[key];
    });
    // A changing server clock alone is not new chart data. Still redraw at a
    // future-bucket boundary, on a filter change, or when the viewport changes.
    var renderKey = JSON.stringify([payload, state.statsAccount, chartWidth,
      rows.map(function (row) { return statsBucketIsFuture(row, chartNow); })]);
    $("statsInfo").textContent = store.message || "";
    if (renderKey === state.statsRenderKey) return;
    var currentTTFT = data.ttftMetric === "first_token_v2";
    var tokenTime = function (row) { return currentTTFT ? row.avgTtftMs : null; };
    $("statsStart").value = data.start;
    $("statsEnd").value = data.end;
    var choice = state.statsAccount || data.account || "";
    var siteScope = choice === "site:cn" || choice === "site:intl";
    var scopeOption = function (value, label) {
      return '<option value="' + esc(value) + '"' + (value === choice ? " selected" : "") + '>' + esc(label) + "</option>";
    };
    setHTMLIfChanged($("statsAccount"), scopeOption("", "Global · All accounts") +
      scopeOption("site:intl", "International sites · All international accounts") + scopeOption("site:cn", "Domestic sites · All domestic accounts") +
      (data.accounts || []).map(function (a) {
        return scopeOption(a.id, a.file + " · " + (a.edition === "intl" ? "International" : "Domestic") + " · " + a.id.slice(0, 6));
      }).join(""));
    if (!siteScope && !(data.accounts || []).some(function (a) { return a.id === choice; })) choice = "";
    $("statsAccount").value = choice;
    state.statsAccount = choice;
    var coverage = (s.tokenSamples || 0) + "/" + (s.requests || 0) + " requests have tokens";
    setHTMLIfChanged($("usageSummary"),
      statsCard("Total tokens", s.tokenSamples ? statsNumber(s.tokens) : "—", "Input " + statsNumber(s.inputTokens) +
        " · Output " + statsNumber(s.outputTokens) + " · Cache reads " + statsNumber(s.cachedTokens), "token") +
      statsCard("Total credits", s.creditSamples ? statsCredit(s.credits) : "—", (s.creditSamples || 0) + "/" + (s.requests || 0) + " requests have credits · usage.credit only", "credit") +
      statsCard("Account calls", statsNumber(s.requests), "Complete " + statsNumber(s.success) + " · Failed / incomplete " + statsNumber(s.failed), "neutral") +
      statsCard("Avg total latency", statsDuration(s.avgLatencyMs), coverage, "token") +
      statsCard("Avg first token (TTFT)", statsDuration(tokenTime(s)), (currentTTFT ? (s.ttftSamples || 0) : 0) + "/" + (s.requests || 0) + " requests have valid samples (new metric)", "first"));
    var sharedBox = {w: chartWidth, h: chartWidth < 520 ? 230 : 280};
    var tooltipRows = function (b) {
      return [
        {label: "Total tokens", color: "token", display: b.tokenSamples ? statsNumber(b.tokens) : (b.requests ? "—" : "0")},
        {label: "Credits", color: "credit", display: b.creditSamples ? statsCredit(b.credits) : (b.requests ? "—" : "0")},
        {label: "Avg total latency", color: "token", display: statsDuration(b.avgLatencyMs)},
        {label: "Avg first token", color: "first", display: statsDuration(tokenTime(b))}
      ];
    };
    var usageOptions = {
      lines: [{label: "Total tokens", color: "token", connectZero: true, value: function (b) { return b.tokenSamples ? b.tokens : null; }}],
      bars: [{label: "Credits", color: "credit", value: function (b) { return b.creditSamples ? b.credits : null; }}],
      title: "",
      leftFormat: function (v) { return v >= 1000000 ? (v / 1000000).toFixed(2) + "M" : statsNumber(v); },
      rightFormat: statsCredit,
      box: sharedBox,
      nowMs: chartNow,
      tooltipRows: tooltipRows
    };
    var latencyRows = data.modelLatency || [];
    var latencyLines = [], latencyBars = [];
    latencyRows.forEach(function (row, index) {
      var color = "m" + (index % 8), label = row.model || "Unnamed model";
      latencyLines.push({label: label, color: color, value: function (_bucket, i) {
        var points = row.avgLatencyMs || [];
        return i < points.length ? points[i] : null;
      }});
      latencyBars.push({label: label, color: color, value: function (_bucket, i) {
        var points = row.avgTtftMs || [];
        return i < points.length ? points[i] : null;
      }});
    });
    setHTMLIfChanged($("latencyLegend"), latencyLines.map(function (s) {
      return '<i class="series-dot ' + s.color + '"></i><span>' + esc(s.label) + "</span>";
    }).join(""));
    var latencyOptions = {
      lines: latencyLines, bars: latencyBars,
      title: "",
      leftFormat: statsDuration,
      box: sharedBox,
      nowMs: chartNow,
      tooltipRows: tooltipRows
    };
    var usageAxes = statsChartAxes(rows, usageOptions), latencyAxes = statsChartAxes(rows, latencyOptions);
    var chartPad = {left: Math.max(usageAxes.left, latencyAxes.left), right: Math.max(usageAxes.right, latencyAxes.right)};
    // Preserve a usable plot even with unusually wide numeric labels on phones.
    var padScale = Math.min(1, (Math.max(260, chartWidth) - 100) / (chartPad.left + chartPad.right));
    chartPad.left = Math.ceil(chartPad.left * padScale);
    chartPad.right = Math.ceil(chartPad.right * padScale);
    usageOptions.plotPad = latencyOptions.plotPad = chartPad;
    hideAllStatsChartHover();
    statsChartControllers = [];
    renderStatsChart("usageChart", rows, usageOptions);
    renderStatsChart("latencyChart", rows, latencyOptions);
    // The breakdown is ranked by model and follows the same time range and account filter; the server already sorts by usage and drops models with no calls.
    var ranked = data.models || [];
    $("statsRange").textContent = data.start + " → " + data.end + " · " + ranked.length + " models with calls";
    setHTMLIfChanged($("statsTable").querySelector("tbody"), ranked.length ? ranked.map(function (m, i) {
      return "<tr><td>" + (i + 1) + "</td><td class='mono'>" + esc(m.model) + "</td><td>" + esc(statsNumber(m.requests)) + "</td><td>" +
        esc(m.success + " / " + m.failed) + "</td><td>" + esc(m.tokenSamples ? statsNumber(m.tokens) : "—") + "</td><td>" +
        esc(m.tokenSamples ? statsNumber(m.inputTokens) + " / " + statsNumber(m.outputTokens) : "—") + "</td><td>" +
        esc(m.tokenSamples ? statsNumber(m.cachedTokens) : "—") + "</td><td>" +
        esc(m.creditSamples ? statsCredit(m.credits) : "—") + "</td><td>" + esc(statsDuration(m.avgLatencyMs)) + "</td><td>" +
        esc(statsDuration(tokenTime(m))) + "</td><td class='hint'>" + esc("Token " + m.tokenSamples + "/" + m.requests +
          " · credits " + m.creditSamples + "/" + m.requests + " · TTFT " + (currentTTFT ? m.ttftSamples : 0) + "/" + m.requests) + "</td></tr>";
    }).join("") : '<tr><td colspan="11" class="empty">No model calls in the selected range</td></tr>');
    state.statsRenderKey = renderKey;
  }

  function loadStats(force) {
    if (!state.apiKey || state.statsPending || (!force && Date.now() - state.statsLastPoll < 10000)) return Promise.resolve();
    state.statsPending = true;
    var version = state.statsVersion, key = state.apiKey;
    var params = ["period=" + encodeURIComponent($("statsPeriod").value || "day")];
    var scope = state.statsAccount || "";
    if (scope.indexOf("site:") === 0) params.push("site=" + encodeURIComponent(scope.slice(5)));
    else if (scope) params.push("account=" + encodeURIComponent(scope));
    ["start", "end"].forEach(function (field) {
      var value = $(field === "start" ? "statsStart" : "statsEnd").value;
      if (value) params.push(field + "=" + encodeURIComponent(value));
    });
    // Background refresh keeps the current layout and any storage warning.
    if (!state.lastStats) $("statsInfo").textContent = "Loading stats…";
    return api("/admin/api/stats?" + params.join("&")).then(function (data) {
      if (version !== state.statsVersion || key !== state.apiKey) return;
      renderStats(data);
      state.statsLastPoll = Date.now();
    }).catch(function (err) {
      if (err && err.status === 400 && state.statsAccount) {
        // When the remembered account was deleted, fall back to the global query instead of failing forever.
        state.statsAccount = "";
        rememberStatsFilter();
        state.statsVersion++;
        return;
      }
      if (version === state.statsVersion && key === state.apiKey) $("statsInfo").textContent = "Failed to load stats; charts keep the previous result. Check the filter dates or retry later.";
    }).then(function () {
      state.statsPending = false;
      if (version !== state.statsVersion && state.apiKey) return loadStats(true);
    });
  }

  function changeStatsFilter(resetDates) {
    state.statsVersion++;
    state.statsLastPoll = 0;
    if (resetDates) {
      $("statsStart").value = "";
      $("statsEnd").value = "";
      state.statsDatesTouched = false;
    }
    state.statsAccount = $("statsAccount").value || "";
    rememberStatsFilter();
    return loadStats(true);
  }

  var SITE_LABEL = { cn: "Domestic site", intl: "International site" };

  function renderStatus(data) {
    state.lastStatus = data;
    var g = data.gateway || {};
    setHTMLIfChanged($("meta"),
      '<a class="chip chip-link" href="https://github.com/CangShui/workbuddy-gateway"' +
      ' target="_blank" rel="noreferrer" title="Open project URL in a new tab">v' + esc(g.version) + '</a>' +
      '<span class="chip">' + esc(g.listen) + '</span>' +
      '<span class="chip">Model source ' + esc(g.modelSource || "unavailable") + '</span>' +
      '<span class="chip">Updated ' + esc(fmtTime(data.updatedAt)) + '</span>');

    renderPoolHealth(data);
    if (state.lastAccountAlerts) renderAccountAlerts(state.lastAccountAlerts);

    setHTMLIfChanged($("gatewayInfo"), kvRow("Version", g.version) +
      kvRow("Listen address", g.listen) +
      kvRow("API auth", g.apiKeyEnabled ? "Enabled" : "Disabled") +
      kvRow("Web console", g.webuiEnabled ? "Enabled" : "Disabled") +
      kvRow("Model source", g.modelSource || "unavailable") +
      kvRow("Snapshot updated", fmtTime(data.updatedAt)));

    renderAccounts(data.accounts || []);
    if (data.stale) showAlert("The status snapshot has not updated for over 15 seconds; showing the previous data. Check the gateway status.",
      false, "/admin/api/status");
  }

  function kvRow(k, v) {
    return '<div class="kv-row"><span class="kv-k">' + esc(k) + '</span><span class="kv-v">' + esc(v) + '</span></div>';
  }

  var STATE_TEXT = {
    active: "Available", cooldown: "Cooldown", paid_exhausted: "Paid exhausted",
    quota_exhausted: "Paid exhausted", expired: "Expired", disabled: "Invalid"
  };
  var STATE_CLASS = {
    active: "ok", cooldown: "warn", paid_exhausted: "warn",
    quota_exhausted: "warn", expired: "warn", disabled: "danger"
  };

  // Cookies only store two display preferences; they never contain the admin Key, credentials, config, or account data.
  // Stats filters (granularity/scope/start and end dates) are stored in a Cookie and restored after logout or reopening the page.
  // Only save dates the user actually changed: leave them empty otherwise and keep the default rolling window, so the default range is never frozen.
  var STATS_FILTER_COOKIE = "gateway_stats_filter";

  function readCookieRaw(name) {
    try {
      var cookies = String(document.cookie || "").split(";");
      for (var i = 0; i < cookies.length; i++) {
        var pair = cookies[i].trim();
        if (pair.indexOf(name + "=") === 0) return decodeURIComponent(pair.slice(name.length + 1));
      }
    } catch (_) { /* Render with defaults when Cookies are disabled; login and queries are unaffected */ }
    return "";
  }

  function writeCookieRaw(name, value) {
    try {
      document.cookie = name + "=" + encodeURIComponent(value) +
        "; Path=/ui; Max-Age=31536000; SameSite=Lax" +
        (window.location.protocol === "https:" ? "; Secure" : "");
      return readCookieRaw(name) === value;
    } catch (_) { return false; }
  }

  function readStatsFilterCookie() {
    var raw = readCookieRaw(STATS_FILTER_COOKIE);
    if (!raw) return null;
    try {
      var saved = JSON.parse(raw);
      if (!saved || typeof saved !== "object") return null;
      var periods = ["hour", "day", "week", "month"];
      return {
        period: periods.indexOf(saved.period) > -1 ? saved.period : "",
        account: typeof saved.account === "string" ? saved.account : "",
        start: typeof saved.start === "string" ? saved.start : "",
        end: typeof saved.end === "string" ? saved.end : ""
      };
    } catch (_) { return null; }
  }

  function writeStatsFilterCookie() {
    return writeCookieRaw(STATS_FILTER_COOKIE, JSON.stringify({
      period: $("statsPeriod").value || "day",
      account: state.statsAccount || "",
      start: state.statsDatesTouched ? ($("statsStart").value || "") : "",
      end: state.statsDatesTouched ? ($("statsEnd").value || "") : ""
    }));
  }

  function initStatsFilter() {
    var saved = readStatsFilterCookie();
    if (!saved) return;
    if (saved.period) $("statsPeriod").value = saved.period;
    state.statsAccount = saved.account;
    if (saved.start || saved.end) {
      $("statsStart").value = saved.start;
      $("statsEnd").value = saved.end;
      state.statsDatesTouched = true;
    }
  }

  function rememberStatsFilter() {
    var remembered = writeStatsFilterCookie();
    var hint = $("statsFilterStatus");
    if (hint) {
      hint.textContent = remembered ? "" : "Stats filter applied, but the browser could not save the Cookie; the selection may not survive a refresh.";
      hint.classList.toggle("hidden", remembered);
    }
    return remembered;
  }

  function readListFilterCookie(name) {
    try {
      var cookies = String(document.cookie || "").split(";");
      for (var i = 0; i < cookies.length; i++) {
        var pair = cookies[i].trim();
        if (pair.indexOf(name + "=") !== 0) continue;
        var value = pair.slice(name.length + 1);
        return value === "1" ? true : value === "0" ? false : null;
      }
    } catch (_) { /* Fall back to showing all when Cookies are disabled; admin login is unaffected */ }
    return null;
  }

  function writeListFilterCookie(name, enabled) {
    try {
      document.cookie = name + "=" + (enabled ? "1" : "0") +
        "; Path=/ui; Max-Age=31536000; SameSite=Lax" +
        (window.location.protocol === "https:" ? "; Secure" : "");
      return readListFilterCookie(name) === enabled;
    } catch (_) { return false; }
  }

  function initListFilters() {
    LIST_FILTERS.forEach(function (filter) {
      $(filter.id).checked = readListFilterCookie(filter.cookie) === true;
      $(filter.id).addEventListener("change", function () { changeListFilter(filter.kind); });
    });
  }

  function reportRestoredListFilters() {
    LIST_FILTERS.forEach(function (filter) {
      reportUIEvent("list_filter_restored", filter.route, 0, traceID(), $(filter.id).checked);
    });
  }

  function changeListFilter(kind) {
    var filter = LIST_FILTERS.find(function (item) { return item.kind === kind; });
    if (!filter) return;
    var trace = traceID();
    var enabled = $(filter.id).checked;
    var remembered = writeListFilterCookie(filter.cookie, enabled);
    var hint = $(filter.statusID);
    hint.textContent = remembered ? "" : "Filter applied, but the browser did not allow saving a Cookie; the state may not survive a refresh.";
    hint.classList.toggle("hidden", remembered);
    reportUIEvent("list_filter_changed", filter.route, 0, trace, enabled);
    if (!remembered) reportUIEvent("list_filter_cookie_failed", filter.route, 0, trace, enabled);
    // The filter applies immediately to loaded data; in-flight responses also render per the current checkbox state.
    if (kind === "accounts") {
      if (state.lastStatus) renderAccounts(state.lastStatus.accounts || []);
    } else if (state.lastModels) {
      renderModels(state.lastModels);
    }
  }

  // ---- Accounts: add via web (reusing the CLI login flow) and delete ---------------------------
  function clearAddAccountArtifacts() {
    $("addAccountQr").removeAttribute("src");
    $("addAccountLink").href = "#";
    $("addAccountLink").textContent = "";
    var copy = $("addAccountCopy");
    if (copy) {
      copy.disabled = true;
      copy.textContent = "Copy link";
    }
  }

  function fallbackCopyText(text, done) {
    var area = document.createElement("textarea");
    area.value = text;
    area.setAttribute("readonly", "");
    area.style.position = "fixed";
    area.style.top = "-1000px";
    area.style.opacity = "0";
    document.body.appendChild(area);
    area.select();
    var ok = false;
    try {
      ok = document.execCommand("copy");
    } catch (_) {
      ok = false;
    }
    document.body.removeChild(area);
    done(ok);
  }

  function copyAddAccountLink() {
    var link = $("addAccountLink");
    var text = (link.textContent || "").replace(/^\s+|\s+$/g, "");
    if (!text) return;
    var button = $("addAccountCopy");
    var done = function (ok) {
      button.textContent = ok ? "Copied" : "Copy failed";
      if (!ok) {
        // In insecure contexts or when the browser denies clipboard permission, at least select the link for manual copying.
        try {
          var range = document.createRange();
          range.selectNodeContents(link);
          var selection = window.getSelection();
          selection.removeAllRanges();
          selection.addRange(range);
        } catch (_) { /* A failed selection does not affect the notice */ }
      }
      window.setTimeout(function () { button.textContent = "Copy link"; }, 1500);
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      try {
        navigator.clipboard.writeText(text).then(function () {
          done(true);
        }, function () {
          fallbackCopyText(text, done);
        });
        return;
      } catch (_) { /* Fall through to the execCommand fallback */ }
    }
    fallbackCopyText(text, done);
  }

  function addAccountRemainingSeconds() {
    if (!state.addAccountDeadline) return 0;
    return Math.max(0, Math.ceil((state.addAccountDeadline - Date.now()) / 1000));
  }

  function fmtAddAccountRemaining(seconds) {
    var m = Math.floor(seconds / 60);
    var s = seconds % 60;
    return m + ":" + (s < 10 ? "0" : "") + s;
  }

  function showAddAccountPending() {
    var left = addAccountRemainingSeconds();
    $("addAccountStatus").textContent = "Scan the QR code with your phone to authorize; on success the credential is saved and loaded automatically." +
      (left > 0 ? " Time remaining " + fmtAddAccountRemaining(left) + "; request a new link after it expires."
        : "The login link is about to expire; please request a new one.");
  }

  function showAddAccountExpired() {
    state.addAccountId = "";
    state.addAccountPoll = 0;
    state.addAccountDeadline = 0;
    clearAddAccountArtifacts();
    $("addAccountStatus").textContent = "The login link has expired; please click \"Get login URL\" again.";
  }

  function openAddAccount() {
    $("addAccountPanel").classList.remove("hidden");
    $("addAccountResult").classList.add("hidden");
    clearAddAccountArtifacts();
    $("addAccountStatus").textContent = "Reading the default credential name…";
    state.addAccountId = "";
    state.addAccountPoll = 0;
    state.addAccountDeadline = 0;
    return api("/admin/api/account-add").then(function (data) {
      $("addFile").value = data.defaultFile || "";
      $("addAccountStatus").textContent = "";
    }).catch(function () {
      $("addAccountStatus").textContent = "Failed to read the default credential name; you can fill it in manually.";
    });
  }

  function closeAddAccount() {
    var id = state.addAccountId;
    state.addAccountId = "";
    state.addAccountPoll = 0;
    state.addAccountDeadline = 0;
    $("addAccountPanel").classList.add("hidden");
    $("addAccountResult").classList.add("hidden");
    clearAddAccountArtifacts();
    if (id) return api("/admin/api/account-cancel", {method: "POST", body: {id: id}}).catch(function () {});
    return Promise.resolve();
  }

  function startAddAccount() {
    var site = $("addSite").value === "intl" ? "intl" : "cn";
    var file = ($("addFile").value || "").replace(/^\s+|\s+$/g, "");
    if (!file) { $("addAccountStatus").textContent = "Please enter a credential name first."; return; }
    if (state.addAccountId) closeAddAccount();
    $("addStart").disabled = true;
    $("addAccountStatus").textContent = "Getting the login URL…";
    return api("/admin/api/account-add", {method: "POST", body: {site: site, file: file}}).then(function (data) {
      state.addAccountId = data.id || "";
      state.addAccountPoll = 0;
      state.addAccountDeadline = data.remainingSeconds
        ? Date.now() + Number(data.remainingSeconds) * 1000
        : (data.expiresAt ? Number(data.expiresAt) * 1000 : 0);
      $("addAccountResult").classList.remove("hidden");
      $("addAccountQr").src = data.qrPng || "";
      $("addAccountLink").href = data.authUrl || "#";
      $("addAccountLink").textContent = data.authUrl || "";
      $("addAccountCopy").disabled = !data.authUrl;
      showAddAccountPending();
      pollAddAccount();
    }).catch(function (err) {
      $("addAccountStatus").textContent = "Failed to get the login URL: " + ((err && err.detail) || "please try again later");
    }).then(function () { $("addStart").disabled = false; });
  }

  function pollAddAccount(force) {
    if (!state.addAccountId || !state.apiKey) return Promise.resolve();
    if (!force && Date.now() - state.addAccountPoll < 2000) return Promise.resolve();
    state.addAccountPoll = Date.now();
    var id = state.addAccountId;
    return api("/admin/api/account-add?id=" + encodeURIComponent(id), {quiet: true}).then(function (data) {
      if (state.addAccountId !== id) return;
      var status = data.status || "pending";
      if (status === "pending") {
        if (typeof data.remainingSeconds === "number") {
          state.addAccountDeadline = Date.now() + data.remainingSeconds * 1000;
        }
        showAddAccountPending();
        return;
      }
      state.addAccountId = "";
      state.addAccountDeadline = 0;
      if (status === "done") {
        $("addAccountStatus").textContent = "Login succeeded; the credential was saved as " + (data.savedAs || "") + " and the account pool reloaded automatically.";
        showAlert("Account added: " + (data.savedAs || ""), true);
        refreshActive();
      } else if (status === "expired") {
        showAddAccountExpired();
      } else if (status === "cancelled") {
        clearAddAccountArtifacts();
        $("addAccountStatus").textContent = "Login cancelled.";
      } else {
        clearAddAccountArtifacts();
        $("addAccountStatus").textContent = "Login failed: " + (data.error || "please retry");
      }
    }).catch(function (err) {
      if (state.addAccountId !== id) return;
      if (err && err.status === 404) {
        showAddAccountExpired();
        return;
      }
      $("addAccountStatus").textContent = "Failed to query login progress; keeping the previous result.";
    });
  }

  function accountRowActions(event) {
    var target = event && event.target;
    if (!target || typeof target.closest !== "function") return;
    var btn = target.closest("[data-account-delete]");
    if (!btn) return;
    var name = btn.getAttribute("data-account-delete");
    if (!name) return;
    if (!window.confirm("Delete credential " + name + " ?\n\n" +
        "This removes the account from the pool and deletes its credential file from the working directory. Stats and historical logs are kept. This cannot be undone.")) return;
    state.policyVersion++;
    api("/admin/api/account-delete", {method: "POST", body: {file: name}}).then(function () {
      showAlert("Deleted credential " + name + "; the account was removed from the pool.", true);
      return refreshActive();
    }).catch(function (err) {
      showAlert("Delete failed: " + ((err && err.detail) || "please try again later"), false);
    });
  }

  function checkinCell(a) {
    var c = a.checkin || {}, now = Date.now();
    var today = new Date(now + 8 * 3600000).toISOString().slice(0, 10);
    var status = c.status || "pending", message = c.message || "";
    if (a.edition === "intl" || status === "unsupported") {
      return '<span class="badge" title="International or enterprise accounts do not participate in personal checkin">—</span>';
    }
    if (c.date !== today) {
      var hour = new Date(now + 8 * 3600000).getUTCHours();
      var minute = new Date(now + 8 * 3600000).getUTCMinutes();
      status = hour * 60 + minute >= 570 ? "missed" : "pending";
      message = status === "missed" ? "Today\u2019s window has passed; no makeup checkin" : "Waiting for today\u2019s random plan (Beijing time)";
    }
    var labels = {pending: "Pending", running: "Pending", success: "Checked in", failed: "Failed", missed: "Missed", unknown: "Unknown"};
    var styles = {success: "ok", failed: "danger", unknown: "warn"};
    var detail = "Beijing time " + today + " · " + message;
    if (c.date === today && c.nextAt) {
      detail += " · Scheduled " + new Date(c.nextAt * 1000).toLocaleString("en-GB", {timeZone: "Asia/Shanghai", hour12: false});
    }
    return '<span class="badge ' + (styles[status] || "") + '" title="' + esc(detail) + '">' +
      (labels[status] || "Unknown") + '</span>';
  }

  function renderAccounts(list) {
    var onlyUsable = $("onlyUsableAccounts").checked;
    if (onlyUsable) {
      list = list.filter(function (account) {
        return account.state === "active" && account.schedulingEnabled !== false;
      });
    }
    var tb = $("accountsTable").querySelector("tbody");
    var focusTarget = focusedPolicyTarget(tb);
    if (!list.length) {
      updateTableBody(tb, '<tr><td colspan="16" class="empty">' +
        (onlyUsable ? "No usable accounts match the filter; uncheck the option to see all accounts." : "No account data yet (has the service written a status snapshot?)") + '</td></tr>');
      return;
    }
    var html = list.map(function (a, i) {
      var st = STATE_TEXT[a.state] || a.state || "-";
      var cls = STATE_CLASS[a.state] || "";
      if (a.schedulingEnabled === false) {
        st = a.state === "active" ? "Disabled" : "Disabled · " + st;
        cls = "warn";
      }
      var site = a.edition === "intl" ? "International site" : "Domestic site";
      return '<tr data-row-key="' + esc(a.path) + '">' +
        "<td>" + (i + 1) + "</td>" +
        "<td>" + policySwitch("account", a.path, a.schedulingEnabled !== false, a.schedulingReason) + "</td>" +
        "<td class='mono'>" + esc(a.path) + "</td>" +
        "<td>" + esc(a.nickname || "-") + "</td>" +
        "<td>" + esc(site) + "</td>" +
        "<td><span class='badge " + cls + "'>" + esc(st) + "</span></td>" +
        "<td>" + esc(fmtTime(a.tokenExpiresAt)) + "</td>" +
        "<td>" + esc(quotaAmount(a, "total")) + "</td>" +
        "<td>" + esc(quotaAmount(a, "used")) + "</td>" +
        "<td>" + esc(quotaAmount(a, "remaining")) + "</td>" +
        "<td>" + esc((a.planLabel || "-") + (a.planStale ? " (stale)" : "")) + "</td>" +
        "<td>" + checkinCell(a) + "</td>" +
        "<td>" + esc(count(a.modelCooldowns)) + "</td>" +
        "<td>" + esc(fmtTime(a.lastRefreshTime)) + "</td>" +
        "<td>" + esc(count(a.refreshFailCount)) + "</td>" +
        "<td><button class=\"ghost danger\" type=\"button\" data-account-delete=\"" +
        esc(a.path) + "\">Delete</button></td>" +
        "</tr>";
    }).join("");
    updateTableBody(tb, html);
    restorePolicyFocus(tb, focusTarget);
  }

  function quotaAmount(account, kind) {
    if (!account.quotaKnown) return "-";
    if (account.quotaUnlimited && (kind === "total" || kind === "remaining")) return "Unlimited";
    var value = kind === "total" ? account.quotaTotal : (kind === "used" ? account.quotaUsed : account.quotaRemaining);
    return count(value);
  }

  // ---- Rendering: models --------------------------------------------------------
  function focusedPolicyTarget(table) {
    var element = document.activeElement;
    return element && table.contains(element) && element.classList.contains("policy-switch") ?
      element.getAttribute("data-target") : null;
  }

  function restorePolicyFocus(table, target) {
    if (target === null) return;
    table.querySelectorAll(".policy-switch").forEach(function (button) {
      if (button.getAttribute("data-target") === target) button.focus({preventScroll: true});
    });
  }

  function policySwitch(kind, target, enabled, reason) {
    return '<button type="button" role="switch" class="policy-switch" data-kind="' + esc(kind) +
      '" data-target="' + esc(target) + '" aria-checked="' + enabled + '" aria-label="' +
      esc((kind === "account" ? "Account " : "Model ") + target + " scheduling switch") +
      '" title="' + esc(reason || (enabled ? "Scheduling allowed" : "Disabled")) + '"' +
      (state.togglePending ? " disabled" : "") + '><span class="switch-track" aria-hidden="true"></span>' +
      (enabled ? "Enabled" : "Disabled") + '</button>';
  }

  function togglePolicy(event) {
    var button = event.target.closest("button.policy-switch");
    if (!button || state.togglePending || !state.apiKey) return;
    var kind = button.getAttribute("data-kind");
    var target = button.getAttribute("data-target");
    var enabled = button.getAttribute("aria-checked") !== "true";
    var endpoint = "/admin/api/" + kind + "-toggle";
    state.togglePending = true;
    state.policyVersion++;
    document.querySelectorAll(".policy-switch").forEach(function (b) { b.disabled = true; });
    return api(endpoint, {method: "POST", body: {target: target, enabled: enabled}})
      .then(function () {
        // Only update the control after a successful save; never mask a failure as applied.
        button.setAttribute("aria-checked", String(enabled));
        button.lastChild.textContent = enabled ? "Enabled" : "Disabled";
        showAlert("Written to config.json; " + (kind === "account" ? "account global scheduling" : "model") +
          (enabled ? " enabled." : " disabled.") + " Running requests are not interrupted.", true);
      }).catch(function () {}).then(function () {
        state.togglePending = false;
        state.policyVersion++;
        document.querySelectorAll(".policy-switch").forEach(function (b) { b.disabled = false; });
        refreshActive();
      });
  }

  function renderModels(data) {
    state.lastModels = data;
    var list = data.models || [];
    var onlyUsable = $("onlyUsable").checked;
    if (onlyUsable) {
      list = list.filter(function (m) { return m.enabled !== false && (m.availableAccounts || 0) > 0; });
    }
    var tb = $("modelsTable").querySelector("tbody");
    var focusTarget = focusedPolicyTarget(tb);
    $("modelPolicyHint").textContent = data.policyMode === "allowlist" ?
      "A model allowlist is active; toggles reflect allowlist rules. The first toggle deletes models.allowlist and converts the current on/off states into models.blocklist; models added later default to enabled." :
      "Model toggles are written to models.blocklist and take effect immediately; disabled models keep their stats records, uncheck the filter to see all models, and the client model catalog hides disabled ones.";
    if (!list.length) {
      updateTableBody(tb, '<tr><td colspan="10" class="empty">' +
        (onlyUsable ? "No usable models match the filter; uncheck the option to see all models." : "No model data yet") + '</td></tr>');
      return;
    }
    var html = list.map(function (m) {
      return '<tr data-row-key="' + esc(m.id) + '">' +
        "<td>" + policySwitch("model", m.id, m.enabled !== false, m.policyReason) + "</td>" +
        "<td class='mono'>" + esc(m.id) + "</td>" +
        rateCell("cn", m.id, m.cnMultiplier) +
        rateCell("intl", m.id, m.intlMultiplier) +
        "<td>" + esc(count(m.availableAccounts)) + "</td>" +
        "<td>" + esc(count(m.requests)) + "</td>" +
        "<td>" + esc(fmtMs(m.avgTtftMs, m.hasTtft && m.ttftMetric === "first_token_v2")) + "</td>" +
        "<td>" + esc(fmtMs(m.avgLatencyMs, m.hasLatency)) + "</td>" +
        "<td>" + esc(fmtTokensM(m.tokens)) + "</td>" +
        "<td>" + esc(m.lastStatus || "-") + "</td>" +
        "</tr>";
    }).join("");
    updateTableBody(tb, html);
    restorePolicyFocus(tb, focusTarget);
    restoreRateDetails(tb);
    if (data.stale) showAlert("The model stats snapshot is too old; showing the previous data.", false, "/admin/api/models");
  }

  // ---- Multiplier sources: all from regular logs; show "No logs" when there is no record -------------------
  function rateKey(site, model) {
    return site + "|" + String(model || "").trim().toLowerCase();
  }

  function rateCell(site, model, text) {
    var label = site === "cn" ? "Domestic" : "International";
    return "<td class='rate-cell'><div class='rate-line'><span class='rate-value'>" + esc(text || "-") +
      "</span><button type='button' class='rate-info' data-rate-site='" + esc(site) +
      "' data-rate-model='" + esc(model) + "' aria-expanded='false' aria-label='View " + label +
      " multiplier data source and update time'>!</button></div>" +
      "<div class='rate-detail hidden' role='region' aria-label='" + label + " multiplier source details'></div></td>";
  }

  function ensureRateDetails() {
    if (state.rateDetails && Date.now() - state.rateDetailsAt < 60000) return Promise.resolve(state.rateDetails);
    if (state.rateDetailsPending) return state.rateDetailsPending;
    state.rateDetailsPending = api("/admin/api/rate-detail").then(function (data) {
      state.rateDetails = data;
      state.rateDetailsAt = Date.now();
      state.rateDetailsPending = null;
      return data;
    }).catch(function (err) {
      state.rateDetailsPending = null;
      throw err;
    });
    return state.rateDetailsPending;
  }

  function rateDetailHTML(payload, site, model) {
    var info = payload && payload.rates ? payload.rates[rateKey(site, model)] : null;
    if (!info || !info.hasLog) {
      return "<p class='rate-none'>No logs</p><p class='hint'>" +
        esc(payload && payload.noLogHint || "No multiplier record for this model in the regular logs.") + "</p>";
    }
    // No longer label a "Status: unknown": the conclusion was actually measured; "unknown" would suggest it was never checked.
    var meta = "<div class='rate-meta'><span>Source: " + esc(info.source) + "</span>" +
      "<span>Updated: " + esc(fmtTime(info.updatedAt)) + "</span></div>";
    var html = meta;
    if (info.explain) html += "<p class='rate-explain'>" + esc(info.explain) + "</p>";
    if (info.unknown && info.refreshHint) {
      html += "<p class='rate-refresh'><strong>How to refresh: </strong>" + esc(info.refreshHint) + "</p>";
    }
    var others = info.others || [];
    if (others.length) {
      html += "<div class='rate-others'><div class='rate-logs-title'>Other records</div><ul>" +
        others.map(function (record) {
          return "<li>" + esc(record.source) + " · " + esc(fmtTime(record.updatedAt)) + " · " +
            esc(record.explain || record.value || "") + "</li>";
        }).join("") + "</ul></div>";
    }
    var raw = (info.records || []).filter(function (record) { return record.logLine; });
    if (raw.length) {
      // Polling rebuilds the table, so the expanded state must be remembered here or every refresh would collapse it.
      var rawOpen = state.openRawLogs[rateKey(site, model)] === true;
      html += "<details class='rate-raw'" + (rawOpen ? " open" : "") +
        "><summary>Raw logs (" + raw.length + " lines, collapsed by default)</summary><ul>" +
        raw.map(function (record) { return "<li><code>" + esc(record.logLine) + "</code></li>"; }).join("") +
        "</ul></details>";
    }
    return html;
  }

  function fillRateDetail(button, panel) {
    var site = button.getAttribute("data-rate-site");
    var model = button.getAttribute("data-rate-model");
    panel.classList.remove("hidden");
    button.setAttribute("aria-expanded", "true");
    panel.innerHTML = "<p class='hint'>Reading logs…</p>";
    ensureRateDetails().then(function (payload) {
      panel.innerHTML = rateDetailHTML(payload, site, model);
      // The details toggle event does not bubble, so bind each one after rendering to remember the expanded state.
      var details = panel.querySelector("details.rate-raw");
      if (details) {
        details.addEventListener("toggle", function () {
          var key = rateKey(site, model);
          if (details.open) {
            state.openRawLogs[key] = true;
          } else {
            delete state.openRawLogs[key];
          }
        });
      }
    }).catch(function () {
      panel.innerHTML = "<p class='hint'>Failed to read logs; please try again later. With no data, the source is not guessed.</p>";
    });
  }

  function toggleRateDetail(event) {
    var button = event.target.closest("button.rate-info");
    if (!button) return;
    var cell = button.closest("td");
    var panel = cell ? cell.querySelector(".rate-detail") : null;
    if (!panel) return;
    var key = rateKey(button.getAttribute("data-rate-site"), button.getAttribute("data-rate-model"));
    if (button.getAttribute("aria-expanded") === "true") {
      button.setAttribute("aria-expanded", "false");
      panel.classList.add("hidden");
      panel.innerHTML = "";
      delete state.openRateKeys[key];
      return;
    }
    state.openRateKeys[key] = true;
    fillRateDetail(button, panel);
  }

  // Replaced cells restore their details; unchanged expanded cells stay intact.
  function restoreRateDetails(table) {
    Array.prototype.forEach.call(table.querySelectorAll("button.rate-info"), function (button) {
      var key = rateKey(button.getAttribute("data-rate-site"), button.getAttribute("data-rate-model"));
      if (!state.openRateKeys[key]) return;
      var cell = button.closest("td");
      var panel = cell ? cell.querySelector(".rate-detail") : null;
      if (panel && (button.getAttribute("aria-expanded") !== "true" || panel.classList.contains("hidden"))) {
        fillRateDetail(button, panel);
      }
    });
  }

  // ---- Rendering: logs --------------------------------------------------------
  function loadLogs(initial) {
    var file = initial ? "" : $("logFile").value;
    var lines = $("logLines").value;
    var q = "/admin/api/logs?lines=" + encodeURIComponent(lines);
    if (file) q += "&file=" + encodeURIComponent(file);
    return api(q).then(function (data) {
      var sel = $("logFile");
      if (initial && data.files) {
        sel.innerHTML = data.files.map(function (f) {
          return '<option value="' + esc(f.name) + '">' + esc(f.name) + '</option>';
        }).join("");
        if (data.file) sel.value = data.file;
      }
      var box = $("logBox");
      if (data.error) { box.textContent = "Failed to read logs: " + data.error; return; }
      var lines = (data.lines || []).slice().reverse();
      box.textContent = lines.length ? lines.join("\n") : "No logs in this file yet.";
      box.scrollTop = 0;
      if (data.truncated) showAlert("The log read reached this run\u2019s budget; showing only some lines. The original log file is unchanged.");
    }).catch(function () {
      if (!initial && file) return loadLogs(true);
    });
  }

  // ---- Rendering: config --------------------------------------------------------
  function loadConfig() {
    var version = ++state.configPreviewVersion;
    return api("/admin/api/config").then(function (data) {
      if (version !== state.configPreviewVersion) return;
      var box = $("configBox");
      if (!data.exists) { box.textContent = "Not found: " + (data.path || "config.json"); return; }
      box.textContent = data.content;
    }).catch(function () {
      // Keep the previous content.
    });
  }

  function configDirty() {
    return state.configMode === "edit" && $("configEditor").value !== state.configOriginal;
  }

  function setConfigBusy(busy) {
    state.configPending = busy;
    ["editConfig", "acknowledgeConfigEdit", "saveConfig", "reloadConfigEditor"].forEach(function (id) {
      $(id).disabled = busy;
    });
    $("configEditor").readOnly = busy;
    $("saveConfig").textContent = busy && state.configMode === "edit" ? "Processing…" : "Validate and save";
  }

  function resetConfigEditor() {
    state.configVersion++;
    state.configMode = "preview";
    state.configRevision = "";
    state.configOriginal = "";
    $("configEditor").value = "";
    $("configEditorStatus").textContent = "";
    $("configWarning").classList.add("hidden");
    $("configEditorPanel").classList.add("hidden");
    $("configBox").classList.remove("hidden");
    $("editConfig").classList.remove("hidden");
    $("configTitle").textContent = "config.json (preview · keys hidden)";
    $("cancelConfigEdit").disabled = false;
    setConfigBusy(false);
  }

  function showConfigWarning() {
    if (state.configPending || state.configMode !== "preview") return;
    hideAlert();
    state.configMode = "warning";
    $("configResult").textContent = "";
    $("editConfig").classList.add("hidden");
    $("configWarning").classList.remove("hidden");
    reportUIEvent("config_edit_warning", "/admin/api/config-edit", 0, traceID());
    $("acknowledgeConfigEdit").focus();
  }

  function exitConfigEdit() {
    if (state.configPending && state.configMode === "edit") return false;
    if (configDirty() && !window.confirm("There are unsaved config changes. Exiting discards them. Continue?")) return false;
    reportUIEvent("config_edit_cancelled", "/admin/api/config-edit", 0, traceID());
    resetConfigEditor();
    hideAlert();
    return true;
  }

  function loadConfigEditor() {
    if (state.configPending || state.configMode === "preview") return Promise.resolve();
    if (configDirty() && !window.confirm("Reloading discards unsaved changes. Continue?")) return Promise.resolve();
    var version = ++state.configVersion;
    var trace = traceID();
    setConfigBusy(true);
    reportUIEvent("config_edit_acknowledged", "/admin/api/config-edit", 0, trace);
    $("configEditorStatus").textContent = "Loading the server config…";
    $("configResult").textContent = "Loading the server config…";
    return api("/admin/api/config-edit?acknowledged=1", {trace: trace}).then(function (data) {
      if (version !== state.configVersion) return;
      state.configRevision = data.revision;
      state.configOriginal = data.content;
      state.configMode = "edit";
      $("configEditor").value = data.content;
      $("configWarning").classList.add("hidden");
      $("configBox").classList.add("hidden");
      $("configEditorPanel").classList.remove("hidden");
      $("editConfig").classList.add("hidden");
      $("configTitle").textContent = "config.json (editing · contains real keys)";
      $("configEditorStatus").textContent = "Raw content loaded. The config and revision are validated before saving; if another page or the server changed the config, the save is rejected.";
      $("configResult").textContent = "";
    }).catch(function (err) {
      if (version !== state.configVersion) return;
      var message = "Load failed: " + (err.detail || err.message) + ". Existing input preserved.";
      $("configEditorStatus").textContent = message;
      $("configResult").textContent = message;
    }).then(function () {
      if (version !== state.configVersion) return;
      setConfigBusy(false);
      if (state.configMode === "edit") $("configEditor").focus();
    });
  }

  function saveConfigEditor() {
    if (state.configPending || state.configMode !== "edit") return Promise.resolve();
    var content = $("configEditor").value;
    var trace = traceID();
    var parsed;
    try {
      parsed = JSON.parse(content);
      if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error("Must be a single JSON object");
      if (new TextEncoder().encode(content).length > 1048576) throw new Error("Exceeds 1 MiB");
    } catch (_) {
      $("configEditorStatus").textContent = "Config not saved: enter a complete JSON object of at most 1 MiB; check quotes, commas, and brackets.";
      reportUIEvent("config_validation_failed", "/admin/api/config-edit", 400, trace);
      return Promise.resolve();
    }
    var nextAdminKey = parsed.gateway && parsed.gateway.adminKey || "";
    var adminKeyChanged = nextAdminKey !== state.apiKey;
    var removal = !nextAdminKey;
    if (removal && !window.confirm("You are clearing the admin Key (or deleting gateway.adminKey). After saving, admin login will be unavailable until it is reset on the server itself. Clear it?")) {
      reportUIEvent("config_edit_cancelled", "/admin/api/config-edit", 0, trace);
      return Promise.resolve();
    }
    if (!window.confirm("Write the entire config to the server\u2019s config.json?\nKeys, rules, prompts, and listening ports hot-reload immediately; existing connections and streaming requests finish. Log retention rules may trigger irreversible deletion." +
        (adminKeyChanged ? "\nThe admin Key changed; you must log in again after saving. Store the new Key safely first." : ""))) {
      reportUIEvent("config_edit_cancelled", "/admin/api/config-edit", 0, trace);
      return Promise.resolve();
    }
    var version = state.configVersion;
    setConfigBusy(true);
    $("cancelConfigEdit").disabled = true;
    $("configEditorStatus").textContent = "Validating and saving…";
    return api("/admin/api/config-edit", {method: "POST", trace: trace,
      body: {content: content, revision: state.configRevision, acknowledged: true, confirmAdminKeyRemoval: removal}
    }).then(function (data) {
      if (version !== state.configVersion) return;
      var message = (data.settings && data.settings.restartRequired ?
        "Config saved, but the listening port has not hot-switched yet; check the server logs." :
        "Config saved; all settings are hot-applied and the listening port is synced.") +
        (data.settings && data.settings.apiPortOverridden ? " The API port is overridden by the -port startup flag." : "");
      resetConfigEditor();
      $("configResult").textContent = message;
      if (data.adminKeyChanged) {
        state.apiKey = "";
        sessionStorage.removeItem(KEY_STORAGE);
        $("apiKey").value = "";
        lockDashboard();
        showAlert(message + (data.adminKeyCleared ? " The admin Key was cleared; reset it on the server itself." : " The admin Key changed; log in again with the new Key."), true);
        setConn("Reset the admin Key or log in again", "");
      } else {
        state.settingsLoadVersion++;
        state.logSettingsVersion++;
        state.policyVersion++;
        loadConfig();
        loadSettings();
      }
    }).catch(function (err) {
      if (version !== state.configVersion) return;
      $("configEditorStatus").textContent = err.status === 409 ?
        "Version conflict: the server config changed. Your edit was not written and the current input is preserved. Review your changes, then reload the latest config." :
        "Save not completed: " + (err.detail || err.message) + ". Current input preserved; check and retry.";
    }).then(function () {
      if (version !== state.configVersion) return;
      $("cancelConfigEdit").disabled = false;
      setConfigBusy(false);
    });
  }

  function renderTasks(data) {
    var labels = {success: "Success", failed: "Failed", partial: "Partial failure", cancelled: "Cancelled",
      skipped: "Skipped", running: "Running", disabled: "Not enabled", unknown: "No run record"};
    var classes = {success: "ok", failed: "danger", partial: "warn", cancelled: "warn", running: "warn"};
    $("tasksInfo").textContent = "Server log timezone: " + (data.timezone || "-") + " (table times shown in the browser timezone) · Refreshed " + fmtTime(data.generatedAt) +
      (data.truncated ? " · The log scan reached its budget; missing records do not mean the task did not run." : " · Run records follow the logs.");
    $("tasksTable").querySelector("tbody").innerHTML = (data.tasks || []).map(function (task) {
      var next = task.nextRunAt ? fmtTime(task.nextRunAt) + (task.nextEstimated ? " (estimated)" : "") :
        (!task.enabled ? "Not enabled" : task.oneShot && task.lastFinishedAt ? "Completed for this startup" : "Waiting for scheduling logs");
      return "<tr><td data-label='Task'>" + esc(task.name) + "</td><td data-label='Enabled'>" + (task.enabled ? "On" : "Off") +
        "</td><td data-label='Schedule' class='task-description'>" + esc(task.schedule) + "</td><td data-label='Last run'>" + esc(fmtTime(task.lastRunAt)) +
        "</td><td data-label='Last completed'>" + esc(fmtTime(task.lastFinishedAt)) + "</td><td data-label='Result'><span class='badge " +
        (classes[task.status] || "") + "'>" + esc(labels[task.status] || "No run record") +
        "</span></td><td data-label='Next expected run'>" + esc(next) + "</td><td data-label='Log notes' class='task-description'>" + esc(task.detail || "No logs for this task in the current service yet, or the logs were cleaned up") +
        (task.logFile ? "<br><span class='mono'>" + esc(task.logFile) + "</span>" : "") + "</td></tr>";
    }).join("");
  }

  function loadLogSettings() {
    var version = ++state.logSettingsVersion;
    $("logRetentionDays").disabled = true;
    $("saveLogSettings").disabled = true;
    $("cleanupLogs").disabled = true;
    return api("/admin/api/log-settings").then(function (data) {
      if (version !== state.logSettingsVersion) return;
      state.logSettings = data;
      $("logRetentionDays").value = data.retentionDays;
    }).catch(function () {}).then(function () {
      if (version !== state.logSettingsVersion) return;
      $("logRetentionDays").disabled = false;
      $("saveLogSettings").disabled = false;
      $("cleanupLogs").disabled = false;
    });
  }

  function saveLogSettings() {
    var days = Number($("logRetentionDays").value);
    if ($("logRetentionDays").value.trim() === "" || !Number.isInteger(days) || days < 0 || days > 36500) {
      showAlert("Retention days must be an integer from 0 to 36500; 0 keeps everything.");
      reportUIEvent("log_validation_failed", "/admin/api/log-settings", 0, traceID());
      return;
    }
    if (days > 0 && !window.confirm("Saving will automatically delete project logs older than " + days + " days; deletion is irreversible. Continue?")) return;
    $("saveLogSettings").disabled = true;
    return api("/admin/api/log-settings", {method: "POST", body: {retentionDays: days}})
      .then(function (data) {
        state.logSettings = data;
        $("logCleanupResult").textContent = "Written to config.json. Retention: " + days + (days === 0 ? " (no automatic cleanup)." : " days; automatic cleanup enabled.");
      }).catch(function () {}).then(function () { $("saveLogSettings").disabled = false; });
  }

  function cleanupExpiredLogs() {
    var days = state.logSettings && state.logSettings.retentionDays;
    if (!days) {
      $("logCleanupResult").textContent = "The current config keeps everything and deletes no logs. Save a positive retention number first.";
      reportUIEvent("log_validation_failed", "/admin/api/log-cleanup", 0, traceID());
      return;
    }
    if (!window.confirm("Expired project logs will be deleted per the saved " + days + "-day retention rule; deletion is irreversible. Continue?")) return;
    $("cleanupLogs").disabled = true;
    return api("/admin/api/log-cleanup", {method: "POST", body: {}}).then(function (data) {
      $("logCleanupResult").textContent = "Deleted " + data.deleted + " log files and reclaimed " + data.deletedBytes +
        " bytes; skipped " + data.skippedActive + " being written, " + data.failed + " failed.";
      loadLogs(true);
    }).catch(function () {}).then(function () { $("cleanupLogs").disabled = false; });
  }

  // ---- Routing and polling --------------------------------------------------------
  function refreshActive() {
    if (!state.apiKey || state.refreshPending || state.togglePending) return Promise.resolve();
    if (state.activeTab === "tasks" && Date.now() - state.tasksLastPoll < 15000) return Promise.resolve();
    if (state.activeTab === "tasks") state.tasksLastPoll = Date.now();
    state.refreshPending = true;
    var policyVersion = state.policyVersion;
    var onErr = function () {
      if (state.activeTab === "overview") {
        renderPoolHealth({stale: true});
      }
      if ($("conn").className.indexOf("danger") === -1) {
        setConn("Connection failed", "danger");
      }
    };
    var pending = Promise.resolve();
    if (state.activeTab === "overview" || state.activeTab === "accounts") {
      pending = api("/admin/api/status").then(function (data) {
        if (!state.togglePending && policyVersion === state.policyVersion) renderStatus(data);
      }).catch(onErr).then(function () {
        // Stats are auxiliary: keep them out of refreshActive's completion chain so slow stats never stall status polling.
        if (state.activeTab === "overview") {
          loadStats(false);
          loadAccountAlerts();
        }
        if (state.activeTab === "accounts") pollAddAccount();
      });
    } else if (state.activeTab === "models") {
      pending = api("/admin/api/models?range=5h").then(function (data) {
        if (!state.togglePending && policyVersion === state.policyVersion) renderModels(data);
      }).catch(onErr);
    } else if (state.activeTab === "logs") {
      pending = loadLogs(false);
    } else if (state.activeTab === "tasks") {
      pending = api("/admin/api/tasks").then(renderTasks).catch(onErr);
    }
    return pending.then(function () {
      state.refreshPending = false;
      if (policyVersion !== state.policyVersion && !state.togglePending) return refreshActive();
    }, function () { state.refreshPending = false; });
  }

  function setTab(name) {
    if (name !== "config" && state.configMode !== "preview" && !exitConfigEdit()) return;
    if (name !== "overview") hideAllStatsChartHover();
    if (name !== "overview" && $("accountAlertTooltip")) $("accountAlertTooltip").hidden = true;
    if (name === "tasks") state.tasksLastPoll = 0;
    state.activeTab = name;
    Array.prototype.forEach.call(document.querySelectorAll(".tab"), function (t) {
      t.classList.toggle("active", t.getAttribute("data-tab") === name);
    });
    Array.prototype.forEach.call(document.querySelectorAll(".panel"), function (p) {
      p.classList.toggle("active", p.id === "panel-" + name);
    });
    if (name === "logs") { loadLogs(true); loadLogSettings(); }
    else if (name === "config") loadConfig();
    else if (name === "settings") loadSettings();
    else refreshActive();
  }

  function renderSettings(data) {
    state.settings = data;
    $("apiKeyEnabled").checked = data.apiKeyEnabled;
    $("modelAPIKey").value = "";
    $("apiKeyState").textContent = data.apiKeyConfigured ? "API Key configured; leaving it blank keeps the current value." : "API Key not configured yet.";
    $("apiPortValue").textContent = data.apiPort;
    $("webPortValue").textContent = data.webPort;
    $("configuredApiPort").value = data.configuredApiPort || data.apiPort;
    $("configuredWebPort").value = data.configuredWebPort || data.webPort;
    $("portSettingsStatus").textContent = (data.restartRequired ? "The port config has not switched yet; check the server logs. " :
      "The configured ports match the actual listeners. ") + (data.apiPortOverridden ? "The API port is overridden by the -port startup flag; the config value will not change the actual listener. " : "") +
      "After editing and saving, the listener hot-switches immediately; existing connections and streaming requests finish. Reopen the console on the new port.";
  }

  function loadSettings() {
    var version = ++state.settingsLoadVersion;
    setSettingsBusy(true);
    return api("/admin/api/settings").then(function (data) {
      if (version === state.settingsLoadVersion) renderSettings(data);
    }).catch(function () {}).then(function () {
      if (version === state.settingsLoadVersion) setSettingsBusy(false);
    });
  }

  function setSettingsBusy(busy) {
    ["apiKeyEnabled", "modelAPIKey", "configuredApiPort", "configuredWebPort", "saveAPISettings", "savePortSettings"]
      .forEach(function (id) { $(id).disabled = busy; });
  }

  function saveAPISettings() {
    var enabled = $("apiKeyEnabled").checked;
    var key = $("modelAPIKey").value;
    if (enabled && !key && !(state.settings && state.settings.apiKeyConfigured)) {
      showAlert("Enter a Key before enabling API Key verification.");
      reportUIEvent("settings_validation_failed", "/admin/api/settings", 0, traceID());
      return;
    }
    setSettingsBusy(true);
    return api("/admin/api/settings", {method: "POST", body: {apiKeyEnabled: enabled, apiKey: key}})
      .then(function (data) {
        renderSettings(data);
        showAlert("Written to config.json; API auth settings take effect immediately. Admin auth is unchanged.", true);
      }).catch(function () {}).then(function () { setSettingsBusy(false); });
  }

  function savePortSettings() {
    var apiPort = Number($("configuredApiPort").value);
    var webPort = Number($("configuredWebPort").value);
    if (!Number.isInteger(apiPort) || !Number.isInteger(webPort) || apiPort < 1 || apiPort > 65535 ||
      webPort < 1 || webPort > 65535 || apiPort === webPort) {
      showAlert("Ports must be integers from 1 to 65535, and the API and Web ports must be different.");
      reportUIEvent("settings_validation_failed", "/admin/api/settings", 0, traceID());
      return;
    }
    setSettingsBusy(true);
    return api("/admin/api/settings", {method: "POST", body: {apiPort: apiPort, webPort: webPort}})
      .then(function (data) {
        renderSettings(data);
        showAlert("Ports written to config.json and hot-switched; existing connections finish. Reopen the console on the new port.", true);
      }).catch(function () {}).then(function () { setSettingsBusy(false); });
  }

  // The admin Key is kept only in this tab's session and stored only after successful verification.
  function saveKey() {
    if (state.configMode !== "preview" && !exitConfigEdit()) return Promise.resolve();
    var key = $("apiKey").value.trim();
    if (!key) { showAlert("Please enter the admin Key."); return Promise.resolve(); }
    state.apiKey = key;
    setConn("Verifying…", "");
    return api("/admin/api/settings").then(function (data) {
      sessionStorage.setItem(KEY_STORAGE, key);
      reportRestoredListFilters();
      renderSettings(data);
      $("tabs").classList.remove("hidden");
      $("dashboard").classList.remove("hidden");
      $("setupPanel").classList.add("hidden");
      showAlert("Admin auth passed.", true);
      refreshActive();
    }).catch(function () {
      // api() already reports 401 / HTTP errors; here we add network-class errors.
      if ($("conn").className.indexOf("danger") === -1) {
        setConn("Connection failed", "danger");
        showAlert("Cannot reach the admin port; check the access address and Web port.");
      }
    });
  }

  function initializeAdmin() {
    $("initializeAdmin").disabled = true;
    return api("/admin/api/setup", {method: "POST", body: {}}).then(function (data) {
      // The raw Key is shown only in this popup; never written to console, logs, or localStorage.
      window.alert("The admin Key was successfully written to config.json:\n\n" + data.adminKey +
        "\n\nStore it safely. This is the only time it is shown automatically; if forgotten, view config.json on the server itself.");
      $("apiKey").value = data.adminKey;
      return saveKey();
    }).catch(function () {}).then(function () { $("initializeAdmin").disabled = false; });
  }

  function discoverSetup() {
    return api("/admin/api/setup").then(function (data) {
      if (data.setupRequired) {
        state.apiKey = "";
        sessionStorage.removeItem(KEY_STORAGE);
        lockDashboard();
        $("setupPanel").classList.remove("hidden");
        $("initializeAdmin").disabled = !data.canInitialize;
        setConn("Waiting for initialization", "");
        if (!data.canInitialize) {
          showAlert("This is not a local connection, so the admin Key cannot be generated automatically on this page. Options:" +
            " ① open this page on the server itself; ② use SSH local port forwarding (ssh -L 8316:127.0.0.1:8316 user@server) and access it;" +
            " ③ edit config.json directly on the server, set gateway.adminKey to a random string, and it takes effect about 1 second after saving.");
        }
      } else if (state.apiKey) {
        saveKey();
      } else {
        lockDashboard();
        setConn("Waiting for admin login", "");
        showAlert("Enter the gateway.adminKey from config.json.");
      }
    }).catch(function () {});
  }

  function initTheme() {
    var saved = localStorage.getItem("wb_theme");
    if (saved) document.documentElement.setAttribute("data-theme", saved);
    $("themeToggle").addEventListener("click", function () {
      var cur = document.documentElement.getAttribute("data-theme");
      var next = cur === "dark" ? "light" : (cur === "light" ? "dark" : (matchMedia("(prefers-color-scheme: dark)").matches ? "light" : "dark"));
      document.documentElement.setAttribute("data-theme", next);
      localStorage.setItem("wb_theme", next);
    });
  }

  function init() {
    // Clean up model API Keys left by older same-origin pages; never mistake them for the admin Key.
    localStorage.removeItem("wb_gateway_api_key");
    initListFilters();
    initStatsFilter();
    ["statsStart", "statsEnd"].forEach(function (id) {
      $(id).addEventListener("change", function () { state.statsDatesTouched = true; });
    });
    $("apiKey").value = state.apiKey;
    initTheme();
    $("saveKey").addEventListener("click", saveKey);
    $("statsApply").addEventListener("click", function () { changeStatsFilter(false); });
    $("statsPeriod").addEventListener("change", function () { changeStatsFilter(true); });
    $("statsAccount").addEventListener("change", function () { changeStatsFilter(false); });
    // The chart viewBox follows the container width; redraw once after window resizes (rAF coalesces bursts).
    var statsResizePending = false;
    window.addEventListener("resize", function () {
      if (!state.lastStats || statsResizePending) return;
      statsResizePending = true;
      (window.requestAnimationFrame || function (fn) { fn(); })(function () {
        statsResizePending = false;
        if (state.lastStats) renderStats(state.lastStats);
      });
    });
    $("logout").addEventListener("click", function () {
      if (state.configMode !== "preview" && !exitConfigEdit()) return;
      resetConfigEditor();
      sessionStorage.removeItem(KEY_STORAGE);
      state.apiKey = "";
      window.location.reload();
    });
    $("initializeAdmin").addEventListener("click", initializeAdmin);
    $("saveAPISettings").addEventListener("click", saveAPISettings);
    $("savePortSettings").addEventListener("click", savePortSettings);
    bindAccountAlertTooltip();
    $("editConfig").addEventListener("click", showConfigWarning);
    $("acknowledgeConfigEdit").addEventListener("click", loadConfigEditor);
    $("cancelConfigWarning").addEventListener("click", exitConfigEdit);
    $("cancelConfigEdit").addEventListener("click", exitConfigEdit);
    $("reloadConfigEditor").addEventListener("click", loadConfigEditor);
    $("saveConfig").addEventListener("click", saveConfigEditor);
    $("configEditor").addEventListener("input", function () {
      $("configEditorStatus").textContent = configDirty() ? "There are unsaved changes. Leaving the config panel asks for confirmation; nothing is saved automatically." : "Content unchanged.";
    });
    window.addEventListener("beforeunload", function (event) {
      if (!configDirty() && !state.configPending) return;
      event.preventDefault();
      event.returnValue = "";
    });
    $("saveLogSettings").addEventListener("click", saveLogSettings);
    $("cleanupLogs").addEventListener("click", cleanupExpiredLogs);
    $("apiKey").addEventListener("keydown", function (e) { if (e.key === "Enter") saveKey(); });
    Array.prototype.forEach.call(document.querySelectorAll(".tab"), function (t) {
      t.addEventListener("click", function () { setTab(t.getAttribute("data-tab")); });
    });
    $("accountsTable").addEventListener("click", togglePolicy);
    $("accountsTable").addEventListener("click", accountRowActions);
    $("addAccountBtn").addEventListener("click", openAddAccount);
    $("addStart").addEventListener("click", startAddAccount);
    $("addCancel").addEventListener("click", closeAddAccount);
    $("addAccountCopy").addEventListener("click", copyAddAccountLink);
    $("modelsTable").addEventListener("click", togglePolicy);
    $("modelsTable").addEventListener("click", toggleRateDetail);
    $("logFile").addEventListener("change", function () { loadLogs(false); });
    $("logLines").addEventListener("change", function () { loadLogs(false); });

    discoverSetup();
    state.timer = setInterval(refreshActive, POLL_MS);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
