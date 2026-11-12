// The Web UI's script (design 4.1): dialogs and confirmations, context menus, time zones, live updates, the
// restore preview and the rule editors. No framework, no inline script: the pages carry data-* attributes.
"use strict";
(function () {
  const $ = (s, r) => (r || document).querySelector(s);
  const $$ = (s, r) => Array.from((r || document).querySelectorAll(s));
  const body = document.body;
  const main = $("main");
  const NS = "http://www.w3.org/2000/svg";

  // ---- texts: /i18n/<lang>, loaded before this script, holds the dictionary of the page's language (design 4.9). A text
  // has {placeholders}; one that has a ".1" variant is written differently for the count 1 (the first number given).
  const I18N = window.KS_I18N || {};
  const rt = (key, args) => {
    let n;
    for (const k in args || {}) if (typeof args[k] === "number") { n = args[k]; break; }
    let s = (n === 1 && I18N[key + ".1"]) || I18N[key] || key;
    for (const k in args || {}) s = s.split("{" + k + "}").join(args[k]);
    return s;
  };

  function el(tag, attrs, ...kids) {
    const e = document.createElement(tag);
    for (const k in attrs || {}) e.setAttribute(k, attrs[k]);
    e.append(...kids);
    return e;
  }
  function icon(name) {
    const s = document.createElementNS(NS, "svg");
    s.setAttribute("class", "i");
    s.setAttribute("aria-hidden", "true");
    s.setAttribute("viewBox", "0 0 24 24");
    const u = document.createElementNS(NS, "use");
    u.setAttribute("href", "/static/icons.svg#i-" + name);
    s.append(u);
    return s;
  }
  function debounce(fn, ms) {
    let t;
    return (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); };
  }

  // ---- time: the server writes UTC; the zone of the settings (or the browser's) is applied here (design 4.7)
  const browserTZ = Intl.DateTimeFormat().resolvedOptions().timeZone;
  function zoneName() {
    const z = body.dataset.tz;
    if (z) { try { new Intl.DateTimeFormat("en", { timeZone: z }); return z; } catch (e) { /* unknown here: use the browser's */ } }
    return browserTZ;
  }
  const formatters = {};
  function parts(ms, zone) {
    const f = formatters[zone] || (formatters[zone] = new Intl.DateTimeFormat("en-GB", { timeZone: zone, hourCycle: "h23",
      year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", second: "2-digit" }));
    const o = {};
    for (const p of f.formatToParts(new Date(ms))) o[p.type] = p.value;
    return o;
  }
  function fmt(ms, zone) {
    const p = parts(ms, zone || zoneName());
    return `${p.year}-${p.month}-${p.day} ${p.hour}:${p.minute}:${p.second}`;
  }
  function renderTimes(root) {
    const zone = zoneName();
    $$("time[datetime]", root).forEach((t) => {
      const d = Date.parse(t.getAttribute("datetime"));
      if (!isNaN(d)) { t.textContent = fmt(d, zone); t.title = zone; }
    });
  }
  // wall: the clock of the zone at instant ms, read as if it were UTC
  const wall = (ms, zone) => { const p = parts(ms, zone); return Date.UTC(+p.year, +p.month - 1, +p.day, +p.hour, +p.minute, +p.second); };
  // parseLocal reads "2026-11-03 22:48[:27]" in the zone; a time that happens twice (clocks set back) or not at all
  // (set forward) takes the earlier reading. null when the text is not a time.
  function parseLocal(text, zone) {
    const m = /^(\d{4})-(\d{2})-(\d{2})[ T](\d{2}):(\d{2})(?::(\d{2}))?$/.exec(text.trim());
    if (!m) return null;
    const guess = Date.UTC(+m[1], +m[2] - 1, +m[3], +m[4], +m[5], +(m[6] || 0));
    if (new Date(guess).getUTCMonth() !== +m[2] - 1) return null;
    return fromWall(guess, zone);
  }
  const utc = (ms) => new Date(ms).toISOString().replace(/\.\d+Z$/, "Z");

  // ---- dialogs and confirmations
  function openDialog(d) {
    if (d && !d.open) d.showModal();
  }
  document.addEventListener("click", (e) => {
    const t = e.target.closest("[data-dialog]");
    if (t) { openDialog($("#" + t.dataset.dialog)); return; }
    const c = e.target.closest("[data-close]");
    if (!c) return;
    const d = c.closest("dialog");
    if (d.leave && !("discard" in c.dataset)) d.leave(); else d.close(); // Esc on a dialog with unsaved edits asks first (d.leave); the cancel button is the answer already
  });
  document.addEventListener("close", (e) => { // a dialog the URL opened takes its parameters with it, so reload does not reopen it
    const names = e.target.dataset && e.target.dataset.clear;
    if (!names) return;
    const u = new URL(location.href);
    names.split(",").forEach((n) => u.searchParams.delete(n));
    history.replaceState(null, "", u);
  }, true);

  const cf = $("#dlg-confirm");
  let pending = null;
  document.addEventListener("submit", (e) => {
    const f = e.target, text = (e.submitter && e.submitter.dataset.confirm) || (f.dataset && f.dataset.confirm);
    if (!text || f.dataset.confirmed) return;
    e.preventDefault();
    pending = { form: f, submitter: e.submitter };
    $("#cf-msg").textContent = text;
    const ok = $("#cf-ok");
    ok.textContent = f.dataset.confirmOk || rt("common.ok");
    ok.className = "btn " + ("danger" in f.dataset ? "danger" : "primary");
    cf.showModal();
  });
  // ask puts a question to the shared confirmation dialog and runs yes() on the button (label ok)
  function ask(msg, ok, yes) {
    pending = { run: yes };
    $("#cf-msg").textContent = msg;
    const b = $("#cf-ok");
    b.textContent = ok;
    b.className = "btn danger";
    cf.showModal();
  }
  if (cf) $("#cf-ok").addEventListener("click", () => {
    const p = pending;
    cf.close();
    if (!p) return;
    if (p.run) { p.run(); return; }
    p.form.dataset.confirmed = "1";
    p.form.requestSubmit(p.submitter || undefined);
    delete p.form.dataset.confirmed;
  });

  // ---- context menus
  const menu = el("div", { id: "ctx", popover: "", class: "pop menu", role: "menu" });
  body.append(menu);
  let opener = null, target = null;
  const pageDir = () => new URLSearchParams(location.search).get("dir") || "";
  function goto(params) {
    const u = new URL(location.href);
    for (const k in params) { if (params[k] === null) u.searchParams.delete(k); else u.searchParams.set(k, params[k]); }
    location.href = u;
  }
  const key = (x) => x.normalize("NFC").toLowerCase();
  const within = (a, b) => !b || a === b || a.startsWith(b + "/"); // a is b or lies inside b ("" is the root)
  // wcItem: the write control of the row's path (a directory, a file, or the one the page shows)
  const wcItem = [rt("js.menu.wc"), "lock", "wc"];
  // fillWC sets the write-control dialog for a path from the data-* of a row, of the folder button or of the dialog
  // itself (r.wcMode is the mode set on the path, empty when it only inherits).
  function fillWC(d, r, isFile) {
    const f = $("form", d), p = r.path || "", inh = r.inh || "single";
    f.elements.path.value = p;
    $('[data-fill="scope"]', d).textContent = d.dataset.folderName + (p ? " / " + p : "");
    $('[data-fill="inherit"]', d).textContent = rt("js.wc.inherit", { mode: rt("js.wc.mode." + inh), from: r.inhFrom ? "/" + r.inhFrom : "/" });
    $$('[data-opt="inherit"]', d).forEach((x) => { x.hidden = !p; });
    $$('[data-opt="free"]', d).forEach((x) => { x.hidden = !!isFile; });
    $("[data-wc-root]", d).hidden = !!p;
    const sel = r.wcMode || (p ? "inherit" : "single");
    [...f.elements.mode].forEach((x) => { x.checked = x.value === sel; });
    const dev = f.elements.device;
    if (r.wcDev && r.wcDev !== "0" && [...dev.options].some((o) => o.value === r.wcDev)) dev.value = r.wcDev;
    const sync = () => { dev.disabled = !dev.options.length || f.elements.mode.value !== "exclusive"; };
    f.onchange = sync;
    sync();
    // devices that hold changes inside the range: saving asks first (design 2.8.11)
    let spans = [];
    try { spans = JSON.parse(d.dataset.held || "[]"); } catch (e) { /* no spans */ }
    const k = key(p), hit = spans.filter((g) => within(k, key(g.P)) || within(key(g.P), k));
    if (hit.length) f.dataset.confirm = rt("js.wc.heldAsk", { n: hit.reduce((a, g) => a + g.N, 0), devices: [...new Set(hit.map((g) => g.D))].join(", ") });
    else delete f.dataset.confirm;
  }
  const restoreLabel = (d) => rt(d.path ? "js.menu.restoreDir" : "js.menu.restoreFolder");
  const MENUS = {
    dir: () => [[rt("js.menu.open"), "open", "open"], [rt("js.menu.restoreAt"), "restore", "restore-at"], wcItem],
    file: () => [[rt("js.menu.versions"), "history", "versions"], [rt("js.menu.restoreAt"), "restore", "restore-at"], wcItem],
    gone: () => [[rt("js.menu.restoreLast"), "restore", "restore-last"], [rt("js.menu.versions"), "history", "versions"]],
    unsynced: () => [[rt("js.menu.detail"), "info", "detail"]],
    blank: (c) => [[restoreLabel(c.row.dataset), "restore", "restore-dir"]],
    "folder-head": (c) => {
      const n = +c.row.dataset.maps;
      return [[restoreLabel(c.row.dataset), "restore", "restore-dir"], wcItem,
        [rt("js.menu.delFolder"), "trash", "del-folder", { danger: true, disabled: n > 0, hint: n > 0 ? rt("js.menu.delFolderHint", { n }) : "" }]];
    },
    "map-row": () => [[rt("js.menu.editMap"), "edit", "edit-map"], [rt("js.menu.delMap"), "trash", "del-map", { danger: true }]],
    "dev-head": () => [[rt("js.menu.revoke"), "trash", "revoke", { danger: true }]],
  };
  const ACTIONS = {
    open: (c) => { location.href = $("a.nm", c.row).href; },
    versions: (c) => goto({ file: c.row.dataset.path }),
    "restore-at": (c) => openRestore(c.row.dataset.path, c.row.dataset.kind === "dir"),
    "restore-dir": (c) => openRestore(c.row.dataset.path || "", !!c.row.dataset.path),
    wc: (c) => {
      const d = $("#dlg-wc");
      $$(".err-msg", d).forEach((x) => x.remove());
      fillWC(d, c.row.dataset, c.row.dataset.kind === "file");
      openDialog(d);
    },
    "restore-last": (c) => { const f = $("#f-restore-last"); f.elements.path.value = c.row.dataset.path; f.requestSubmit(); },
    detail: (c) => {
      const d = $("#dlg-unsynced"), r = c.row.dataset;
      $('[data-fill="path"]', d).textContent = r.path;
      $('[data-fill="msg"]', d).textContent = r.msg;
      $('[data-fill="device"]', d).textContent = r.device;
      $('[data-fill="advice"]', d).textContent = r.advice;
      const t = $('[data-fill="since"]', d);
      t.setAttribute("datetime", new Date(+r.since * 1000).toISOString());
      renderTimes(d);
      openDialog(d);
    },
    "edit-map": (c) => {
      const d = $("#dlg-map-edit"), r = c.row.dataset, f = $("form", d);
      f.elements.folder.value = r.folder;
      f.elements.path.value = r.path;
      f.elements.adopt.checked = !!r.adopt;
      $('[data-fill="name"]', d).textContent = r.name;
      openDialog(d);
    },
    "del-map": (c) => {
      const f = $("#f-del-map"), n = +c.row.dataset.locks || 0;
      f.elements.folder.value = c.row.dataset.folder;
      f.dataset.confirm = f.dataset.base + (n ? " " + rt("js.locks.release", { n }) : "");
      f.requestSubmit();
    },
    revoke: () => $("#f-revoke").requestSubmit(),
    "del-folder": () => $("#f-del-folder").requestSubmit(),
  };

  // openMenu shows the menu of a row (or of a button or an empty area) at a point of the viewport.
  function openMenu(row, kind, x, y, from) {
    const items = (MENUS[kind] || (() => []))({ row }).filter(Boolean);
    if (!items.length) return;
    menu.replaceChildren();
    if (row.dataset.path && /^(dir|file|gone|unsynced)$/.test(kind)) menu.append(el("div", { class: "mh" }, row.dataset.path + (kind === "dir" ? "/" : "")));
    for (const [label, ic, act, o] of items) {
      const b = el("button", { type: "button", class: "mi" + (o && o.danger ? " danger" : ""), role: "menuitem" }, icon(ic), label);
      b.dataset.act = act;
      if (o && o.disabled) b.disabled = true;
      menu.append(b);
      if (o && o.hint) menu.append(el("div", { class: "hint" }, o.hint));
    }
    menu.setAttribute("aria-label", row.dataset.name || rt("js.menu.actions"));
    target = { row };
    opener = from || row;
    menu.showPopover();
    const w = menu.offsetWidth, h = menu.offsetHeight;
    menu.style.setProperty("left", Math.max(8, Math.min(x, innerWidth - w - 8)) + "px");
    menu.style.setProperty("top", (y + h > innerHeight - 8 ? Math.max(8, y - h) : y) + "px");
    const rowEl = row.closest("tr");
    if (rowEl) { rowEl.classList.add("menu-open"); const m = $(".more", rowEl); if (m) m.classList.add("on"); }
    const first = $(".mi:not(:disabled)", menu);
    if (first) first.focus({ focusVisible: false });
  }
  menu.addEventListener("toggle", (e) => {
    if (e.newState !== "closed") return;
    $$(".menu-open").forEach((r) => r.classList.remove("menu-open"));
    $$(".more.on").forEach((b) => b.classList.remove("on"));
    if (opener && opener.focus && !$("dialog[open]")) opener.focus({ focusVisible: false });
    flush();
  });
  menu.addEventListener("click", (e) => {
    const b = e.target.closest(".mi");
    if (!b || b.disabled) return;
    const c = target;
    opener = null; // the action takes the focus (a dialog), not the row
    menu.hidePopover();
    ACTIONS[b.dataset.act](c);
  });
  menu.addEventListener("keydown", (e) => {
    const items = $$(".mi:not(:disabled)", menu), i = items.indexOf(document.activeElement);
    if (e.key === "ArrowDown") items[(i + 1) % items.length].focus();
    else if (e.key === "ArrowUp") items[(i - 1 + items.length) % items.length].focus();
    else return;
    e.preventDefault();
  });
  addEventListener("scroll", () => { if (menu.matches(":popover-open")) menu.hidePopover(); }, true);

  let pressedAt = 0;
  document.addEventListener("contextmenu", (e) => {
    if (e.target.closest("dialog, .pop")) return;
    const row = e.target.closest("tr[data-kind]");
    const area = row ? null : e.target.closest("[data-menu-area]");
    if (!row && !area) return;
    e.preventDefault();
    if (Date.now() - pressedAt < 800) return; // the long press has opened it already
    if (row) openMenu(row, row.dataset.kind, e.clientX, e.clientY);
    else openMenu(area, area.dataset.menuArea, e.clientX, e.clientY);
  });
  document.addEventListener("click", (e) => {
    const b = e.target.closest("[data-menu]");
    if (!b) return;
    const row = b.closest("tr[data-path], tr[data-folder]") || b;
    const kind = b.dataset.menu === "row" ? row.dataset.kind : b.dataset.menu;
    const r = b.getBoundingClientRect();
    openMenu(row, kind, r.right - 200, r.bottom + 2, b);
  });
  document.addEventListener("keydown", (e) => { // Shift+F10 and the menu key open the menu of the row that has the focus
    if (e.key !== "ContextMenu" && !(e.shiftKey && e.key === "F10")) return;
    const row = e.target.closest("tr[data-kind]");
    if (!row) return;
    e.preventDefault();
    const r = $(".more", row).getBoundingClientRect();
    openMenu(row, row.dataset.kind, r.right - 200, r.bottom + 2, $("a", row) || row);
  });
  // long press (touch screens that send no contextmenu event): 500 ms, moving less than 10 px
  let press = null;
  document.addEventListener("touchstart", (e) => {
    const row = e.target.closest("tr[data-kind]"), t = e.touches[0];
    if (!row || e.touches.length > 1) return;
    cancelPress();
    const p = press = { x: t.clientX, y: t.clientY, row, ready: false };
    p.timer = setTimeout(() => { p.ready = true; }, 500);
  }, { passive: true });
  // lifting the finger after a long press must not click the row (a link) under it
  let swallow = false;
  document.addEventListener("click", (e) => { if (swallow) { swallow = false; e.preventDefault(); e.stopPropagation(); } }, true);
  const cancelPress = () => { if (press) clearTimeout(press.timer); press = null; };
  // the menu opens after the finger lifts: a popover opened under a finger that is still down is light-dismissed by its pointerup
  document.addEventListener("touchend", () => {
    const p = press;
    cancelPress();
    if (p && p.ready) { // the compatibility mouse events of this touch follow at once and would light-dismiss the menu, so it opens after them
      pressedAt = Date.now(); swallow = true;
      setTimeout(() => openMenu(p.row, p.row.dataset.kind, p.x, p.y + 24), 150);
    }
    setTimeout(() => { swallow = false; }, 600);
  });
  document.addEventListener("touchcancel", cancelPress);
  document.addEventListener("touchmove", (e) => {
    const t = e.touches[0];
    if (press && Math.hypot(t.clientX - press.x, t.clientY - press.y) > 10) cancelPress();
  }, { passive: true });

  // ---- tips (design 4.8): a [data-tip] trigger names its hidden content (.tip-c); one element shows it on hover, on a
  // tap, and on keyboard focus of the trigger or of an element described by the content (the row's name). WCAG 1.4.13:
  // Escape hides it, the pointer can move onto it, it stays until the pointer has left both.
  const tip = el("div", { class: "tip", role: "tooltip" });
  tip.hidden = true;
  body.append(tip);
  let tipFor = null, tipTimer = 0, tipFocusAt = -1e9;
  function showTip(trigger) {
    clearTimeout(tipTimer);
    if (tipFor === trigger && !tip.hidden) return;
    const c = document.getElementById(trigger.dataset.tip);
    if (!c) return;
    tipFor = trigger;
    tip.replaceChildren(...Array.from(c.childNodes, (n) => n.cloneNode(true)));
    tip.hidden = false;
    placeTip(trigger);
  }
  function placeTip(trigger) {
    const r = trigger.getBoundingClientRect(), w = tip.offsetWidth, h = tip.offsetHeight;
    const x = Math.max(8, Math.min(r.left - 8, innerWidth - w - 8));
    const y = r.bottom + 6 + h > innerHeight - 8 && r.top - 6 - h >= 8 ? r.top - 6 - h : r.bottom + 6;
    tip.style.setProperty("left", x + "px");
    tip.style.setProperty("top", y + "px");
  }
  function hideTip(later) {
    clearTimeout(tipTimer);
    if (later) tipTimer = setTimeout(() => hideTip(false), 300);
    else { tip.hidden = true; tipFor = null; }
  }
  const tipTrigger = (n) => { // the trigger of a focused element: itself, or the one whose content describes it
    if (n.matches("[data-tip]")) return n;
    const c = n.getAttribute("aria-describedby") && document.getElementById(n.getAttribute("aria-describedby"));
    return c && c.classList.contains("tip-c") ? c.closest("[data-tip]") : null;
  };
  document.addEventListener("pointerover", (e) => {
    if (e.pointerType === "touch") return;
    const t = e.target.closest("[data-tip]");
    if (t) showTip(t);
    else if (tip.contains(e.target)) clearTimeout(tipTimer);
  });
  document.addEventListener("pointerout", (e) => {
    if (e.pointerType === "touch" || tip.hidden) return;
    const to = e.relatedTarget;
    if (to && (tip.contains(to) || (tipFor && tipFor.isConnected && tipFor.contains(to)))) return;
    if (e.target.closest("[data-tip]") || tip.contains(e.target)) hideTip(true);
  });
  document.addEventListener("click", (e) => {
    const t = e.target.closest("[data-tip]");
    if (t && e.pointerType !== "mouse") { if (tipFor === t && !tip.hidden) hideTip(false); else showTip(t); } // a tap
    else if (!tip.contains(e.target) && !t) hideTip(false);
  });
  document.addEventListener("focusin", (e) => {
    const t = tipTrigger(e.target);
    if (t && e.target.matches(":focus-visible")) { showTip(t); tipFocusAt = performance.now(); }
  });
  document.addEventListener("focusout", (e) => { if (tipFor && tipTrigger(e.target) === tipFor) hideTip(false); });
  document.addEventListener("keydown", (e) => { if (e.key === "Escape" && !tip.hidden) hideTip(false); }, true);
  // Scrolling hides the tip, except the scroll the browser makes to bring a just-focused element into view: that one
  // moves the tip along with its trigger.
  addEventListener("scroll", () => {
    if (tip.hidden) return;
    if (tipFor && tipFor.isConnected && performance.now() - tipFocusAt < 500) placeTip(tipFor);
    else hideTip(false);
  }, true);
  // A live update that replaced the trigger leaves no pointerout or focusout behind: the tip goes with it.
  function dropStaleTip() { if (!tip.hidden && !(tipFor && tipFor.isConnected)) hideTip(false); }

  // ---- notifications
  const notif = $("#notif");
  document.addEventListener("click", (e) => {
    const b = e.target.closest("[data-notif]");
    if (!b || !notif) return;
    notif.showPopover();
    const g = b.dataset.notif && document.getElementById(b.dataset.notif);
    if (g) g.scrollIntoView({ block: "start" });
  });
  if (notif) notif.addEventListener("toggle", (e) => { if (e.newState === "closed") flush(); });

  // ---- live updates: /events says that something changed; the marked regions of the page are fetched and replaced
  const liveURL = body.dataset.liveUrl || location.href;
  let es = null, lastGen = null, dirty = false;
  const busy = () => !!$("dialog[open]") || menu.matches(":popover-open");
  function flush() { if (dirty) refresh(); }
  // The decision dialog stays open while a device carries out the decisions it was given: it takes the new list in
  // place as long as the user has not started choosing rows.
  async function refreshHeld() {
    const d = $("dialog#dlg-held[open]");
    if (!d || !$("[data-waiting]", d) || $$(".held-rows select", d).some((s) => s.value)) return;
    let r;
    try { r = await fetch(location.href, { credentials: "same-origin" }); } catch (e) { return; }
    if (!r.ok) return;
    const nu = $("#dlg-held .dlg-b", new DOMParser().parseFromString(await r.text(), "text/html"));
    const cur = $(".dlg-b", d);
    if (nu && cur && d.open) cur.replaceChildren(...Array.from(nu.childNodes).map((n) => document.importNode(n, true)));
    renderTimes(d);
  }
  async function refresh() {
    if (busy()) { dirty = true; refreshHeld(); return; }
    dirty = false;
    let r;
    try { r = await fetch(liveURL, { credentials: "same-origin", headers: main && main.dataset.folder ? { "X-Live": "1" } : {} }); } catch (e) { return; }
    if (r.redirected && new URL(r.url).pathname === "/login") { location.href = "/login"; return; }
    if (!r.ok || busy()) { dirty = true; return; }
    const doc = new DOMParser().parseFromString(await r.text(), "text/html");
    for (const cur of $$("[data-live]")) {
      const nu = $(`[data-live="${cur.dataset.live}"]`, doc);
      if (!nu) continue;
      if (cur === notif && notif.matches(":popover-open")) { dirty = true; continue; } // only the count changes while it is read
      cur.replaceChildren(...Array.from(nu.childNodes).map((n) => document.importNode(n, true)));
      renderTimes(cur);
    }
    dropStaleTip();
  }
  // The hint "content updated" is for changes that the listing shows. The event says that the folder changed; the
  // server says whether that touched the directory on the page (a direct child, or a subdirectory that came or went).
  let checked = main && main.dataset.seq, latest = checked, checking = false;
  async function listingChanged(seq) {
    const hint = $("[data-reload]");
    latest = String(seq);
    if (!hint || !hint.hidden || checking || latest === checked) return;
    checking = true;
    let ok = false;
    try {
      const q = new URLSearchParams({ since: checked, dir: pageDir() });
      if (new URLSearchParams(location.search).get("deleted")) q.set("deleted", "1");
      for (const r of $$('tr[data-kind="dir"]')) q.append("d", r.dataset.name);
      const r = await fetch("/folders/" + main.dataset.folder + "/changed?" + q, { credentials: "same-origin" });
      if (!r.ok) return;
      const out = await r.json();
      if (out.changed) hint.hidden = false; else checked = String(out.seq);
      ok = true;
    } catch (e) { /* the next event asks again */ } finally { checking = false; }
    if (ok && hint.hidden && latest !== checked) listingChanged(latest); // events came in while the server was asked
  }
  function connect() {
    if (es || document.hidden || !notif) return;
    es = new EventSource("/events");
    es.onopen = () => $("[data-conn]").classList.remove("on");
    es.onmessage = (e) => {
      const d = JSON.parse(e.data);
      if (main && main.dataset.folder && d.seq) listingChanged(d.seq[main.dataset.folder]);
      if (lastGen !== null && d.gen !== lastGen) refresh();
      lastGen = d.gen;
    };
    es.onerror = () => {
      $("[data-conn]").classList.add("on");
      if (es.readyState === EventSource.CLOSED) { // refused (the session has ended?): the refresh finds out; try again later
        es = null;
        refresh();
        setTimeout(connect, 10000);
      }
    };
  }
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) { if (es) es.close(); es = null; return; }
    connect();
    refresh();
  });
  document.addEventListener("click", (e) => { if (e.target.closest("[data-reload]")) location.href = liveURL; });

  // ---- restore to a point in time: two time axes with draggable lines, and a preview (design 4.6)
  // One selected time is shown on two axes that never move each other. The day axis (top) runs from the oldest state a
  // restore can reach (left) to now (right edge, written "Now" outside the axis); its line stops only at each 00:00 up to
  // today's and at the oldest point. The time axis (bottom) always shows one calendar day, 00:00 to 24:00, and its line
  // moves by the minute inside that day; the part after now and the part before the earliest point are hatched and
  // cannot be picked. The tint is the data that stays: left of the line. The server sends the ticks: the ends of the
  // minutes with a change in the scope; the day axis marks each change at its own position. Dragging only moves the lines and
  // changes some text; the preview is asked for when the state under the line changes and the line rests. Only the
  // newest request counts, an older one is aborted. The preview's glob filter is applied by the server, which holds the
  // whole list. Every text this code writes is in the dictionary (rt, above), with {placeholders}.

  // fromWall: the instant at which the clock of the zone reads w (as UTC); a reading that happens twice takes the earlier
  // instant, one that does not happen the nearest candidate.
  function fromWall(w, zone) {
    const c = [-864e5, 864e5].map((d) => w - (wall(w + d, zone) - (w + d))).sort((a, b) => a - b);
    return c.find((x) => wall(x, zone) === w) ?? c[0];
  }

  function restoreDialog(rd) {
    const MIN = 6e4, H = 36e5, D = 864e5;
    // tunables
    const MINOR_PX = 4;           // the closest two short ticks may be
    const LABEL_GAP = 8;          // free space after a label
    const STEP_WAIT = 300;        // keys and step buttons: ask for the preview once they rest this long
    const TYPE_WAIT = 400;        // typing: the same
    const WEEK = 7;               // PageUp / PageDown on the day axis move this many days
    const DAY_BASE = 23, TOD_BASE = 33; // baselines inside the axes
    const LONG_DAYS = 30;         // a day axis longer than this labels its ticks MM-DD instead of the day only
    const FILTER_WAIT = 300;      // the glob filter: ask once typing rests this long

    const LANG = document.documentElement.lang || "en";
    const zone = zoneName();
    const form = rd.querySelector("form");
    const input = form.elements.time, go = $("[data-restore-go]", rd), goText = $("span", go);
    const axes = $("[data-axes]", rd), amsg = $(".axes-msg", axes);
    const mkAxis = (el) => ({ el, svg: $("svg", el), cur: $(".axis-cur", el), shade: $(".axis-shade", el), w: 0, x: null, t: null });
    const A = { day: mkAxis($('[data-axis="day"]', rd)), tod: mkAxis($('[data-axis="tod"]', rd)) };
    const rel = $("[data-rel]", rd), pv = $("[data-preview]", rd), steps = $$("[data-step]", rd), note2 = $("[data-note2]", rd);
    const bar = el("div", { class: "progress", hidden: "" });
    // the glob filter of the preview: it lives in the head line of a preview that lists files; the server applies it
    const filterEl = el("input", { type: "text", class: "pv-filter mono", autocomplete: "off", spellcheck: "false", maxlength: "200", placeholder: rt("js.filter"), "aria-label": rt("js.filter") });
    const flt = () => filterEl.value.trim();
    const keepDays = +rd.dataset.keepDays;
    // fmtT: the time as the box shows it; whole minutes (the ticks, the dragged line) without seconds, the two ends with them
    const fmtT = (ms) => fmt(ms, zone).slice(0, ms % MIN ? 19 : 16);
    let T = null;     // the ticks of the scope, all in ms: {now, from, ticks} (null: none loaded, none to show, or failed)
    let stops = [];   // where the day axis' line may stop: the 00:00 of each day from the earliest point's day to today
    let win = null;   // the time axis' window {a, b}: the calendar day of the day axis' line
    let dayMs = null; // where the day axis' line is: a stop; only the day axis, the box and the step buttons change it
    let sel = null;   // the selected time (ms); null: the box holds no time
    let opened = null; // the time the dialog opened at, while the person has not chosen another
    let shown = null, asked = null, askedMs = 0, result = null, ctl = null, waitT = 0, typeT = 0, filterT = 0; // shown, asked: state key + "|" + filter
    const nowMs = () => (T ? T.now : Date.now());
    const clamp = (v, lo, hi) => Math.min(hi, Math.max(lo, v));
    const lin = (a, b, w) => ({ x: (ms) => (ms - a) / (b - a) * w, t: (x) => a + x / w * (b - a) });
    function bsearch(k, ms) { // the last index whose value is at or before ms; -1 before the first
      if (!k.length || ms < k[0]) return -1;
      let lo = 0, hi = k.length - 1;
      while (lo < hi) { const mid = (lo + hi + 1) >> 1; if (k[mid] <= ms) lo = mid; else hi = mid - 1; }
      return lo;
    }
    const dayStart = (ms, plus = 0) => { const p = parts(ms, zone); return fromWall(Date.UTC(+p.year, +p.month - 1, +p.day + (plus || 0)), zone); };
    // winFor: the window of the time axis for a time: its calendar day, 00:00 to the next 00:00.
    const winFor = (ms) => ({ a: dayStart(ms), b: dayStart(ms, 1) });
    // dayOf: where the day axis' line stands for a time: the 00:00 of its day (never before the first stop)
    const dayOf = (v) => Math.max(dayStart(clamp(v, T.from, T.now)), stops[0]);

    // ---- tick levels, aligned to the calendar of the zone: days of the month 1, 1+d, ..., months m, minutes within a day
    const DAY_LEVELS = [1, 2, 5, 10, 15].map((d) => ({ d, ms: d * D })).concat([1, 3, 6, 12].map((m) => ({ m, ms: m * 30.44 * D })));
    const TOD_LEVELS = [15, 30, 60, 120, 180, 240, 360, 720].map((m) => ({ min: m, ms: m * MIN }));
    function ticksOf(lv, a, b) {
      const out = [], p = parts(a, zone);
      if (lv.m) {
        for (let i = 0; ; i++) {
          const w = Date.UTC(+p.year, +p.month - 1 + i, 1), ms = fromWall(w, zone);
          if (ms > b) return out;
          if (ms >= a && new Date(w).getUTCMonth() % lv.m === 0) out.push(ms);
        }
      }
      for (let i = 0; ; i++) {
        const w = Date.UTC(+p.year, +p.month - 1, +p.day + i), ms = fromWall(w, zone);
        if (ms > b) return out;
        if (lv.d) {
          const dom = new Date(w).getUTCDate();
          if (ms >= a && (dom - 1) % lv.d === 0 && (lv.d === 1 || dom <= 31 - lv.d / 2)) out.push(ms);
          continue;
        }
        for (let m = 0; m < 1440; m += lv.min) { const x = ms + m * MIN; if (x > b) break; if (x >= a) out.push(x); } // DST days: approximate
      }
    }
    const ctx = document.createElement("canvas").getContext("2d");
    let font = "";
    const textW = (s, bold) => {
      if (!font) font = "11px " + getComputedStyle(document.body).getPropertyValue("--sans");
      ctx.font = (bold ? "600 " : "") + font;
      return ctx.measureText(s).width;
    };
    // dates on the day axis are numbers only, the same in every language: the day of the month (30, 1, 2), or MM-DD
    // (12-01) when the axis spans more than LONG_DAYS days, where the day alone would not say which month
    let long = false;
    const dayText = (ms) => { const p = parts(ms, zone); return long ? p.month + "-" + p.day : String(+p.day); };
    const labelW = () => textW(long ? "00-00" : "00");
    const finest = (levels, k, pxMin) => levels.find((lv) => lv.ms * k >= pxMin) || levels[levels.length - 1];
    const px = (x) => Math.round(x) + 0.5;
    const esc = (s) => s.replace(/&/g, "&amp;").replace(/</g, "&lt;");
    // labels: placed in order of priority (then right to left); one that would overlap a placed one is left out
    function labelsSvg(list, w) {
      const placed = [];
      let out = "";
      list.sort((p, q) => p.pri - q.pri || q.x - p.x);
      for (const l of list) {
        const lw = textW(l.s, l.cls === "now"), x0 = l.end ? l.x - lw : l.x, x1 = x0 + lw;
        if (x0 < 2 || x1 > w - 2 || placed.some(([a, b]) => x0 < b + LABEL_GAP && x1 + LABEL_GAP > a)) continue;
        placed.push([x0, x1]);
        out += `<text class="lb ${l.cls || ""}" x="${Math.round(x0)}" y="12">${esc(l.s)}</text>`;
      }
      return out;
    }
    function marksSvg(x, a, b, w, y) { // one short line per pixel column that holds a change between a and b
      let d = "";
      const col = new Uint8Array(w + 1), n = T.ticks.length - 1;
      for (let i = Math.max(1, bsearch(T.ticks, a)); i < n && T.ticks[i] <= b; i++) {
        const c = Math.round(x(T.ticks[i]));
        if (c >= 0 && c <= w && !col[c]) { col[c] = 1; d += `M${Math.min(c, w - 1) + 0.5} ${y}v8`; }
      }
      return `<path class="mk" d="${d}"/>`;
    }
    function daysSvg(x, w, y) { // the day axis: one mark per change at its own position; changes in one pixel column share a mark that is taller with more of them
      const col = new Map(), n = T.ticks.length - 1;
      for (let i = 1; i < n; i++) { const k = Math.min(Math.round(x(T.ticks[i])), w - 1); if (k >= 0) col.set(k, (col.get(k) || 0) + 1); }
      let d = "";
      for (const [k, c] of col) d += `M${k + 0.5} ${y}v${c > 3 ? 8 : c > 1 ? 6 : 4}`;
      return `<path class="mk" d="${d}"/>`;
    }
    // the day axis: the day of the earliest point to now; ticks for days, dates on the tall ones. With a long span only
    // as many dates as fit (the finest level whose spacing holds a label), so they never overlap.
    function renderDay() {
      const ax = A.day, w = ax.w = ax.el.clientWidth;
      if (!T || !w) { ax.svg.innerHTML = ""; ax.x = ax.t = null; return; }
      const d0 = stops[0], s = lin(d0, T.now, w), k = w / (T.now - d0);
      ax.x = s.x; ax.t = s.t;
      long = T.now - T.from > LONG_DAYS * D;
      const label = finest(DAY_LEVELS, k, labelW() + LABEL_GAP), minor = finest(DAY_LEVELS, k, MINOR_PX);
      const lt = ticksOf(label, d0, T.now), ls = new Set(lt);
      let tall = "", short = "";
      lt.forEach((ms) => { tall += `M${px(s.x(ms))} 13V${DAY_BASE}`; });
      tall += `M${px(0)} 13V${DAY_BASE}`;
      if (minor.ms < label.ms) ticksOf(minor, d0, T.now).forEach((ms) => { if (!ls.has(ms)) short += `M${px(s.x(ms))} ${DAY_BASE - 5}V${DAY_BASE}`; });
      const labels = lt.map((ms) => ({ x: s.x(ms) + 3, s: dayText(ms), pri: 1 }));
      if (!ls.has(d0)) labels.push({ x: 3, s: dayText(d0), pri: 1 }); // the first day is labelled whatever the level
      ax.svg.setAttribute("viewBox", `0 0 ${w} 34`);
      ax.svg.innerHTML = `<path class="tk" d="${tall}"/><path class="tk s" d="${short}"/><path class="base" d="M0 ${DAY_BASE + 0.5}H${w}"/>`
        + daysSvg(s.x, w, DAY_BASE + 3) + labelsSvg(labels, w);
    }
    // the time axis: the day, hours, a tall tick at 00:00; the 00:00 label sits inside the axis and 24:00 is written beside it
    function renderTod() {
      const ax = A.tod, w = ax.w = ax.el.clientWidth;
      if (!T || !w) { ax.svg.innerHTML = ""; ax.x = ax.t = null; return; }
      const s = lin(win.a, win.b, w), k = w / (win.b - win.a);
      ax.x = s.x; ax.t = s.t;
      const label = finest(TOD_LEVELS.filter((lv) => lv.min >= 60), k, textW("00:00") + LABEL_GAP), minor = finest(TOD_LEVELS, k, MINOR_PX);
      const mid = ticksOf({ d: 1 }, win.a, win.b).filter((ms) => ms < win.b), ms0 = new Set(mid);
      const lt = ticksOf(label, win.a, win.b).filter((ms) => ms < win.b), ls = new Set(lt);
      let tall = "", med = "", short = "", na = "";
      mid.forEach((ms) => { tall += `M${px(Math.max(0, s.x(ms)))} 0V${TOD_BASE}`; });
      lt.forEach((ms) => { if (!ms0.has(ms)) med += `M${px(s.x(ms))} 16V${TOD_BASE}`; });
      ticksOf(minor, win.a, win.b).forEach((ms) => { if (!ms0.has(ms) && !ls.has(ms)) short += `M${px(s.x(ms))} ${TOD_BASE - 5}V${TOD_BASE}`; });
      const x0 = clamp(Math.round(s.x(T.from)), 0, w), x1 = clamp(Math.round(s.x(T.now)), 0, w);
      if (x0 > 0 || x1 < w) na = `<pattern id="rs-na" width="6" height="6" patternUnits="userSpaceOnUse" patternTransform="rotate(45)"><path d="M0 0V6"/></pattern>`
        + (x0 > 0 ? `<rect class="na" x="0" y="0" width="${x0}" height="50"/>` : "")
        + (x1 < w ? `<rect class="na" x="${x1}" y="0" width="${w - x1}" height="50"/>` : ""); // before the earliest point, after now: hatched
      const hf = (ms) => { const p = parts(ms, zone); return p.hour + ":" + p.minute; };
      const labels = lt.map((ms) => ({ x: s.x(ms) + 3, s: hf(ms), pri: 2 }));
      ax.svg.setAttribute("viewBox", `0 0 ${w} 50`);
      ax.svg.innerHTML = na + `<path class="tk" d="${tall}"/><path class="tk" d="${med}"/><path class="tk s" d="${short}"/><path class="base" d="M0 ${TOD_BASE + 0.5}H${w}"/>`
        + marksSvg(s.x, win.a, win.b, w, TOD_BASE + 3) + labelsSvg(labels, w);
    }
    function setWin(nw) {
      if (win && nw.a === win.a && nw.b === win.b) return;
      win = nw;
      renderTod();
    }
    // todRange: what the time axis can pick: its day, inside the restorable range; never the next 00:00
    const todRange = () => [Math.max(win.a, T.from), Math.min(win.b - MIN, T.now)];
    function render() { renderDay(); renderTod(); place(); }
    function place() { // the two lines and the tint of the data that stays, left of each line: only transforms, no layout
      const hide = sel === null;
      for (const ax of [A.day, A.tod]) ax.cur.hidden = ax.shade.hidden = hide;
      if (!T || hide || !A.day.x || !A.tod.x) return;
      const xd = A.day.x(dayMs), xt = A.tod.x(clamp(sel, win.a, win.b));
      A.day.cur.style.transform = `translateX(${xd}px)`;
      A.tod.cur.style.transform = `translateX(${xt}px)`;
      A.day.shade.style.transform = `translateX(${xd - A.day.w}px)`;
      A.tod.shade.style.transform = `translateX(${xt - A.tod.w}px)`;
    }

    // ---- selection: the time, its state key, and whether the preview on the screen still belongs to it
    // idx: the last tick at or before ms; -1 before the first. From the last change to now nothing differs: one state.
    function idx(ms) { const i = bsearch(T.ticks, ms); return i < 0 ? -1 : Math.min(i, T.ticks.length - 2); }
    const keyOf = (ms) => (ms === null ? "bad" : T ? idx(ms) : ms);
    function relText(ms) {
      const m = Math.round((nowMs() - ms) / MIN);
      if (m < 0) return rt("js.future");
      if (m === 0) return rt("js.now");
      const d = Math.floor(m / 1440), h = Math.floor((m % 1440) / 60), mi = m % 60;
      if (d) return h ? rt("js.ago_dh", { d, h }) : rt("js.ago_d", { d });
      if (h) return mi ? rt("js.ago_hm", { h, m: mi }) : rt("js.ago_h", { h });
      return rt("js.ago_m", { m: mi });
    }
    // setSel shows the time ms everywhere: the box (unless the person is typing in it), the distance from now, both lines.
    // It is for the day axis, typing and the step buttons: the day axis' line and the time axis go to the day of ms.
    // The time axis calls it with tod set, which leaves the day axis' line and the window alone. It sends nothing:
    // commit asks for the preview.
    function setSel(ms, typed, tod) {
      sel = ms;
      if (!typed && ms !== null) input.value = fmtT(ms);
      rel.textContent = ms === null ? "" : relText(ms);
      if (T && ms !== null) {
        if (!tod) { dayMs = dayOf(ms); setWin(winFor(dayMs)); }
        const v = clamp(ms, T.from, T.now), vt = fmtT(ms) + ", " + relText(ms);
        for (const ax of [A.day, A.tod]) { ax.el.setAttribute("aria-valuenow", Math.round(v / 1000)); ax.el.setAttribute("aria-valuetext", vt); }
      }
      place();
      stale();
    }
    const setTod = (ms) => setSel(clamp(ms, ...todRange()), false, true);
    function stale() {
      const s = !(result && (result.fail || result.hint)) && keyOf(sel) + "|" + flt() !== shown;
      pv.classList.toggle("stale", s);
      go.disabled = s || !result || !result.n;
    }
    function abort() { if (ctl) { ctl.abort(); ctl = null; } }
    // commit asks for the preview of the selected time unless the screen shows it or it is already asked for.
    function commit() {
      clearTimeout(waitT);
      const st = keyOf(sel), key = st + "|" + flt();
      if (key === asked) return;
      abort();
      if (key === shown) { asked = null; bar.hidden = true; return; }
      asked = key;
      askedMs = sel;
      if (st === "bad") { show(key, { bad: true }); return; }
      if (T && st === T.ticks.length - 2) { show(key, { hint: true }); return; } // from the last change to now: nothing to ask
      bar.hidden = false;
      const ms = sel, c = ctl = new AbortController();
      const q = new URLSearchParams({ path: form.elements.path.value, time: utc(ms), filter: flt() });
      fetch(`${location.pathname}/restore-preview?${q}`, { credentials: "same-origin", signal: c.signal })
        .then((r) => r.ok ? r.text() : Promise.reject(r.status))
        .then((html) => {
          if (c !== ctl) return;
          const doc = new DOMParser().parseFromString(html, "text/html"), cnt = $("[data-count]", doc);
          show(key, { ms, n: cnt ? +cnt.dataset.count : 0, nodes: Array.from(doc.body.childNodes).map((n) => document.importNode(n, true)) });
        })
        .catch((e) => { if (c === ctl && !(e && e.name === "AbortError")) show(null, { fail: true }); });
    }
    function show(key, res) {
      shown = key; asked = null; result = res; ctl = null;
      bar.hidden = true;
      const r = res;
      let nodes = r.nodes;
      if (!nodes) { // a result of this script: the hint, the box holds no time, or the preview did not load
        nodes = [el("div", { class: "preview-h" + (r.bad ? " warn" : ""), "aria-live": "polite" }, icon(r.bad ? "warn" : "history"),
          el("span", {}, r.hint ? rt("js.hint") : r.bad ? rt("js.bad", { ex: "2026-11-03 22:48:27" }) : rt("js.fail")))];
      } else if (!nodes.some((n) => n.classList && n.classList.contains("first-n")) && r.ms < nowMs() - keepDays * D) {
        nodes.splice(1, 0, el("div", { class: "first-n" }, icon("info"), el("span", {}, rt("js.old")))); // after the head line
      }
      const typing = document.activeElement === filterEl;
      pv.replaceChildren(bar, ...nodes);
      if (r.n) { $(".preview-h", pv).append(filterEl); if (typing) filterEl.focus(); }
      renderTimes(pv);
      goText.textContent = r.n ? rt("js.go", { n: r.n }) : r.bad || r.fail || r.hint ? rt("common.restore") : rt("js.go0");
      stale();
    }

    // ---- what a pointer position means on each axis
    function dayAt(x) { // the stop nearest to x: the 00:00 of a day
      const ms = A.day.t(clamp(x, 0, A.day.w)), i = Math.max(0, bsearch(stops, ms)), j = Math.min(stops.length - 1, i + 1);
      return ms - stops[i] <= stops[j] - ms ? stops[i] : stops[j];
    }
    function todAt(x) { // the time under x, to the minute, inside the window and the restorable range; never out of the day
      return clamp(Math.round(A.tod.t(clamp(x, 0, A.tod.w)) / MIN) * MIN, ...todRange());
    }
    // ---- input: pointer on either axis, keys on either axis, the step buttons, typing
    let drag = null, raf = 0, lastX = 0;
    function move(x) { if (drag.kind === "day") setSel(Math.max(dayAt(x), T.from)); else setTod(todAt(x)); }
    for (const kind of ["day", "tod"]) {
      const ael = A[kind].el;
      ael.addEventListener("pointerdown", (e) => {
        if (!T || !A[kind].x || e.button > 0) return;
        const r = ael.getBoundingClientRect(); // read once: no layout reads while the line moves
        drag = { kind, left: r.left + 1, start: sel, win, day: dayMs, moved: false, touch: e.pointerType === "touch" };
        ael.setPointerCapture(e.pointerId);
        ael.focus({ preventScroll: true });
        if (!drag.touch) move(e.clientX - drag.left); // a touch may still turn into a scroll: wait for a move
        e.preventDefault();
      });
      ael.addEventListener("pointermove", (e) => {
        if (!drag) return;
        drag.moved = true;
        lastX = e.clientX - drag.left;
        if (!raf) raf = requestAnimationFrame(() => { raf = 0; if (drag) move(lastX); });
      });
      ael.addEventListener("pointerup", (e) => {
        if (!drag) return;
        if (!drag.moved) move(e.clientX - drag.left); // a tap puts the line there
        drag = null;
        commit();
      });
      ael.addEventListener("pointercancel", () => { if (drag) { const d = drag; drag = null; setSel(d.start); if (d.kind === "tod") { dayMs = d.day; setWin(d.win); place(); } } }); // it was a scroll
    }
    function later(ms, tod) { if (tod) setTod(ms); else setSel(ms); clearTimeout(waitT); waitT = setTimeout(commit, STEP_WAIT); }
    function stepChange(d, tod) { // to the start of the previous or next state; the last one is now. Keys on the time axis stay inside its day
      const n = T.ticks.length - 1, j = clamp(idx(sel ?? T.now) + d, 0, n), ms = j >= n - 1 ? T.now : T.ticks[j], [lo, hi] = todRange();
      if (tod && (ms < lo || ms > hi)) return;
      later(ms, tod);
    }
    A.day.el.addEventListener("keydown", (e) => {
      if (!T) return;
      const n = stops.length - 1, at = bsearch(stops, dayMs), below = Math.max(0, at - 1), above = Math.min(n, at + 1);
      const j = { ArrowLeft: below, ArrowDown: below, ArrowRight: above, ArrowUp: above, PageDown: at - WEEK, PageUp: at + WEEK, Home: 0, End: n }[e.key];
      if (j === undefined) return;
      e.preventDefault();
      later(Math.max(stops[clamp(j, 0, n)], T.from));
    });
    A.tod.el.addEventListener("keydown", (e) => {
      if (!T) return;
      const [lo, hi] = todRange(), v = clamp(sel ?? T.now, lo, hi), p = parts(v, zone), hour = fromWall(Date.UTC(+p.year, +p.month - 1, +p.day, +p.hour), zone);
      const ch = { ArrowLeft: -1, ArrowDown: -1, ArrowRight: 1, ArrowUp: 1 }[e.key];
      const to = { PageDown: hour < v ? hour : hour - H, PageUp: hour + H, Home: lo, End: hi }[e.key];
      if (ch === undefined && to === undefined) return;
      e.preventDefault();
      if (ch) stepChange(ch, true); else later(to, true);
    });
    steps.forEach((b) => b.addEventListener("click", () => { if (T) stepChange(+b.dataset.step, false); })); // the buttons may cross days
    input.addEventListener("input", () => {
      clearTimeout(typeT);
      typeT = setTimeout(() => { setSel(parseLocal(input.value, zone), true); commit(); }, TYPE_WAIT); // a full date and time: both lines go there
    });
    filterEl.addEventListener("input", () => { stale(); clearTimeout(filterT); filterT = setTimeout(commit, FILTER_WAIT); });
    filterEl.addEventListener("keydown", (e) => { if (e.key === "Enter") e.preventDefault(); }); // Enter must not submit the restore
    let lastW = 0;
    new ResizeObserver(() => { if (axes.clientWidth !== lastW) { lastW = axes.clientWidth; render(); } }).observe(axes);
    form.addEventListener("submit", (e) => { // the server reads UTC: send the time the person typed as UTC
      const ms = parseLocal(input.value, zone);
      if (ms === null) { e.preventDefault(); setSel(null, true); commit(); return; }
      input.value = utc(ms);
    });
    rd.addEventListener("close", () => { abort(); clearTimeout(waitT); clearTimeout(typeT); clearTimeout(filterT); });

    // ---- the note under the preview: time zone, retention, and the earliest point
    function fillNote() {
      const [a, b] = rt("js.note2", { tz: zone, how: rt(body.dataset.tz ? "js.fixed" : "js.follow") }).split("{link}");
      const l = el("a", { href: "/settings" }, rt("nav.settings"));
      note2.replaceChildren(a, l, b, T ? rt("js.note3", { d: keepDays, from: fmtT(T.from) }) : rt("js.note3b", { d: keepDays }));
    }

    // ---- opening: the dialog is at now, the axes when their ticks arrive
    async function loadTicks(path) {
      let t = null, failed = false;
      try {
        const r = await fetch(`${location.pathname}/restore-ticks?` + new URLSearchParams({ path }), { credentials: "same-origin" });
        if (!r.ok) failed = true; else t = await r.json();
      } catch (e) { failed = true; }
      if (form.elements.path.value !== path || !rd.open) return;
      const atOpen = sel !== null && sel === opened;
      if (t && !t.none && t.ticks.length >= 2) T = { from: t.from * 1000, now: t.now * 1000, ticks: t.ticks.map((s) => s * 1000) };
      if (T) { // what was asked for or shown by the minute now has a state: the same state is not asked for again
        if (shown !== null && result && result.ms !== undefined) shown = idx(result.ms) + "|" + flt();
        if (asked !== null && !asked.startsWith("bad")) asked = idx(askedMs) + "|" + flt();
        if (atOpen) sel = T.now; // still at the opening time: now is the server's
        stops = ticksOf({ d: 1 }, dayStart(T.from), T.now);
        dayMs = dayOf(sel ?? T.now);
        win = winFor(dayMs);
        A.day.el.setAttribute("aria-valuemin", Math.round(T.from / 1000));
        A.day.el.setAttribute("aria-valuemax", Math.round(T.now / 1000));
        A.tod.el.setAttribute("aria-valuemin", Math.round(T.from / 1000));
        A.tod.el.setAttribute("aria-valuemax", Math.round(T.now / 1000));
      } else {
        amsg.textContent = rt(failed ? "js.axisFail" : "js.axisNone");
        amsg.hidden = false;
      }
      axes.classList.toggle("off", !T);
      for (const ax of Object.values(A)) ax.el.tabIndex = T ? 0 : -1;
      steps.forEach((b) => { b.disabled = !T; });
      fillNote();
      render();
      setSel(sel, !atOpen); // the distance from now and the state under the line follow the server's clock
      commit();
    }
    // open: path is the scope; isDir says how to write it (null: the page wrote it already, with the time that was typed)
    function open(path, isDir) {
      form.elements.path.value = path;
      if (isDir !== null) $('[data-fill="scope"]', rd).textContent = rd.dataset.folderName + (path ? " / " + path + (isDir ? "/" : "") : "");
      const keep = isDir === null && input.value; // a form sent back with an error keeps its time
      abort();
      T = win = dayMs = null; stops = []; shown = asked = result = null;
      filterEl.value = "";
      axes.hidden = false;
      axes.classList.add("off");
      amsg.hidden = true;
      steps.forEach((b) => { b.hidden = false; b.disabled = true; });
      pv.hidden = false;
      pv.replaceChildren(bar);
      fillNote();
      if (!rd.open) rd.showModal();
      render();
      if (keep) { opened = null; setSel(parseLocal(input.value, zone), true); commit(); }
      else { opened = Date.now(); setSel(opened); show(null, { hint: true }); } // the dialog opens at now: nothing to ask yet
      lockNote(path);
      loadTicks(path);
    }
    // an exclusive, or a single writer that wrote a moment ago, inside the range (or around it) does not stop a restore:
    // say so (design 4.5). Both entries (the menu, and the page reopened after an error) come through open().
    function lockNote(path) {
      const note = $("[data-locknote]", rd);
      let locks = [];
      try { locks = JSON.parse(note.dataset.locks || "[]"); } catch (e) { /* no note */ }
      const p = key(path || "");
      const hit = locks.find((l) => within(key(l.P), p) || within(p, key(l.P)));
      note.hidden = !hit;
      if (hit) note.textContent = rt(hit.S ? "js.restoreOwner" : "js.restoreLocked", { device: hit.D });
    }
    return { open };
  }
  const restore = $("#dlg-restore") && restoreDialog($("#dlg-restore"));
  const openRestore = (path, isDir) => restore.open(path, isDir);

  // ---- decision dialog: different actions chosen row by row may leave the scope a mix of two devices' versions
  document.addEventListener("change", (e) => {
    const sel = e.target.closest(".held-rows select");
    if (!sel) return;
    const f = sel.form, picked = new Set(Array.from(f.querySelectorAll("select[name=a]"), (x) => x.value).filter(Boolean));
    $("[data-mixwarn]", f).hidden = picked.size < 2;
  });

  // ---- version history: the restore asks in its own row
  document.addEventListener("click", (e) => {
    const a = e.target.closest("[data-ask], [data-ask-cancel]");
    if (!a) return;
    const td = a.closest("td"), asking = "ask" in a.dataset;
    $("form.ask", td).hidden = !asking;
    a.closest("tr").classList.toggle("asking", asking);
  });

  // ---- exclusion-rule editors: a highlighted layer behind the textarea, errors from the server's own parser
  function ruleEditor(box) {
    const ta = $("textarea", box), pre = $("pre", box), list = box.nextElementSibling;
    const LH = 20, PAD = 6;
    let errs = new Map(), seq = 0;
    box.classList.add("js");
    function draw() {
      const cur = ta.value.slice(0, ta.selectionStart).split("\n").length;
      pre.replaceChildren(...ta.value.split("\n").map((t, i) =>
        el("span", { class: "l" + (t[0] === "#" ? " c" : "") + (errs.has(i + 1) ? " e" : "") + (errs.has(i + 1) && i + 1 === cur ? " cur" : "") }, t)));
      $$("[data-line]", list).forEach((d) => d.classList.toggle("cur", +d.dataset.line === cur));
      pre.scrollTop = ta.scrollTop;
      pre.scrollLeft = ta.scrollLeft;
    }
    const validate = debounce(async () => {
      const mine = ++seq;
      let out;
      try {
        const r = await fetch("/rules/validate", { method: "POST", body: ta.value, headers: { "Content-Type": "text/plain" }, credentials: "same-origin" });
        if (!r.ok) return;
        out = await r.json();
      } catch (e) { return; }
      if (mine !== seq) return;
      errs = new Map(out.errors.map((x) => [x.line, x.msg]));
      list.replaceChildren(...out.errors.map((x) => el("div", { class: "err-msg", "data-line": x.line }, rt("js.ruleLine", { n: x.line, msg: x.msg }))));
      ta.setAttribute("aria-invalid", errs.size ? "true" : "false");
      draw();
    }, 300);
    ta.addEventListener("input", () => { draw(); validate(); preview.edited(); });
    // One main button: "Preview" while the rules differ from what was previewed, then "Save" (or "Save and delete N files").
    // The server checks the token of the preview at the save. Without script the button saves and the server answers
    // with the preview first, as it does for a save without a token.
    const form = box.closest("form"), go = $("[data-rulesgo]", form), hitsBox = $("[data-hitsbox]", form);
    const saved0 = (box.dataset.saved || "").replace(/^\n/, "");
    const preview = (() => {
      let shown = hitsBox.firstElementChild ? ta.value : null, asking = 0; // the text the preview on the page is for
      const label = () => {
        const h = $("[data-hits]", hitsBox), n = h ? +h.dataset.hits : 0;
        go.className = "btn " + (shown === ta.value && n ? "danger" : "primary");
        go.textContent = shown === ta.value ? (n ? rt("js.saveDelete", { n }) : rt("common.save")) : (ta.value === saved0 ? rt("common.save") : rt("js.preview"));
        go.disabled = false;
      };
      const self = {
        edited() { // the box changed: an old preview is out of date
          asking++;
          if (shown !== ta.value) { shown = null; hitsBox.replaceChildren(); }
          label();
        },
        async ask() {
          const mine = ++asking, text = ta.value, old = $("[data-hits-filter]", hitsBox), filter = old ? old.value : "";
          go.disabled = true;
          let html;
          try {
            const r = await fetch(form.getAttribute("action") + "/preview", { method: "POST", body: new URLSearchParams({ rules: text, filter }), credentials: "same-origin" });
            html = r.ok ? await r.text() : null;
          } catch (e) { html = null; }
          if (mine !== asking) return;
          if (html === null) { // the rules are invalid or the server is out of reach: the errors list says which
            hitsBox.replaceChildren(el("div", { class: "err-msg" }, rt("js.previewFail")));
            shown = null;
          } else {
            const doc = new DOMParser().parseFromString(html, "text/html");
            const typing = old && document.activeElement === old;
            hitsBox.replaceChildren(...Array.from(doc.body.childNodes).map((n) => document.importNode(n, true)));
            const nf = $("[data-hits-filter]", hitsBox); // the page draws it hidden: without script the filter would do nothing
            if (nf) { nf.hidden = false; if (typing) { nf.focus(); nf.setSelectionRange(nf.value.length, nf.value.length); } }
            shown = text;
          }
          label();
        },
        needed: () => shown !== ta.value && ta.value !== saved0,
      };
      return self;
    })();
    // the glob filter of the list: the server filters (it holds the whole list), so a change asks for the preview again
    const refilter = debounce(() => preview.ask(), 300);
    hitsBox.addEventListener("input", (e) => { if (e.target.matches("[data-hits-filter]")) refilter(); });
    hitsBox.addEventListener("keydown", (e) => { if (e.key === "Enter" && e.target.matches("[data-hits-filter]")) e.preventDefault(); }); // not a save
    go.addEventListener("click", (e) => {
      if (!preview.needed()) return;
      e.preventDefault();
      preview.ask();
    });
    preview.edited();
    // Edits that were not saved are lost when the dialog closes (cancel, Esc), after asking. The text to go back to
    // is the saved one: after a refused save the textarea holds the refused text, not the saved one. A textarea
    // drops one leading newline of its source, the attribute does not.
    const dlg = box.closest("dialog"), saved = (box.dataset.saved || "").replace(/^\n/, "");
    if (dlg && box.dataset.saved !== undefined) {
      const dirty = () => ta.value !== saved;
      dlg.leave = () => { if (dirty()) ask(rt("js.discard"), rt("js.discardOk"), () => dlg.close()); else dlg.close(); };
      dlg.addEventListener("cancel", (e) => { if (dirty()) { e.preventDefault(); dlg.leave(); } });
      dlg.addEventListener("close", () => {
        seq++; // a validation still under way would show errors of the discarded text
        ta.value = saved;
        preview.edited();
        errs = new Map();
        list.replaceChildren();
        ta.setAttribute("aria-invalid", "false");
        draw();
      });
    }
    ta.addEventListener("scroll", () => { pre.scrollTop = ta.scrollTop; pre.scrollLeft = ta.scrollLeft; });
    ["keyup", "click", "focus"].forEach((n) => ta.addEventListener(n, draw));
    ta.addEventListener("mousemove", (e) => {
      const line = Math.floor((e.clientY - ta.getBoundingClientRect().top + ta.scrollTop - PAD) / LH) + 1;
      ta.title = errs.has(line) ? rt("js.ruleLine", { n: line, msg: errs.get(line) }) : "";
    });
    draw();
    if (ta.value) validate();
  }

  // ---- settings: the time zone card
  const tc = $("[data-tzcard]");
  if (tc) {
    const sel = $("select", tc), radios = $$("input[name=tz_mode]", tc), sample = $("[data-sample]", tc);
    const saved = tc.dataset.tz;
    $("[data-browser-tz]", tc).textContent = browserTZ;
    let zones = [];
    try { zones = Intl.supportedValuesOf("timeZone"); } catch (e) { /* an older browser: the saved zone and UTC */ }
    // Two columns, the name and the UTC offset now (summer time included): the options are set in a monospace font
    // (style.css) and the names padded with no-break spaces.
    const offset = (z) => {
      try {
        const o = new Intl.DateTimeFormat("en", { timeZone: z, timeZoneName: "longOffset" }).formatToParts(Date.now()).find((p) => p.type === "timeZoneName").value;
        return o === "GMT" ? "+00:00" : o.replace("GMT", "");
      } catch (e) { return ""; }
    };
    // Sorted by the offset as a number (-12:00 before +05:30 before +14:00), then by name.
    const minutes = (o) => { const m = /^([+-])(\d\d):(\d\d)$/.exec(o); return m ? (m[1] === "-" ? -1 : 1) * (+m[2] * 60 + +m[3]) : 0; };
    const names = [...new Set(["UTC", ...zones, saved || browserTZ])].map((z) => ({ z, o: offset(z) }))
      .sort((a, b) => minutes(a.o) - minutes(b.o) || (a.z < b.z ? -1 : a.z > b.z ? 1 : 0));
    const width = Math.max(...names.map((n) => n.z.length));
    sel.replaceChildren(...names.map((n) => el("option", { value: n.z }, n.z.padEnd(width, "\u00a0") + "\u00a0\u00a0" + n.o)));
    sel.value = saved || browserTZ;
    const update = () => {
      const fixed = radios.find((r) => r.checked).value === "fixed";
      sel.disabled = !fixed;
      const zone = fixed ? sel.value : browserTZ;
      sample.textContent = fmt(Date.now(), zone);
      sample.title = zone;
    };
    radios.forEach((r) => r.addEventListener("change", update));
    sel.addEventListener("change", update);
    update();
  }

  // ---- small things: copy buttons, the enrollment countdown, the result bar
  document.addEventListener("click", async (e) => {
    const b = e.target.closest("[data-copy]");
    if (!b) return;
    const code = document.getElementById(b.dataset.copy);
    try { await navigator.clipboard.writeText(code.textContent); } catch (err) {
      const r = document.createRange();
      r.selectNodeContents(code);
      getSelection().removeAllRanges();
      getSelection().addRange(r);
      document.execCommand("copy");
    }
    const old = b.lastChild.textContent;
    b.lastChild.textContent = rt("js.copied");
    setTimeout(() => { b.lastChild.textContent = old; }, 1500);
  });
  $$("[data-countdown]").forEach((c) => {
    const end = +c.dataset.countdown * 1000;
    const tick = () => {
      const s = Math.max(0, Math.round((end - Date.now()) / 1000));
      c.textContent = s ? `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}` : rt("js.expired");
      if (!s) clearInterval(t);
    };
    const t = setInterval(tick, 1000);
    tick();
  });
  const toast = $("[data-toast]");
  if (toast) {
    const u = new URL(location.href);
    ["ok", "restored", "failed"].forEach((n) => u.searchParams.delete(n));
    if (u.href !== location.href) history.replaceState(null, "", u);
    setTimeout(() => toast.remove(), 5000);
    toast.addEventListener("click", (e) => { if (e.target.closest("[data-toast-close]")) toast.remove(); });
  }

  // ---- start
  renderTimes(document);
  $$("[data-ruleedit]").forEach(ruleEditor);
  $$("dialog[data-autoopen]").forEach((d) => {
    if (restore && d.id === "dlg-restore") restore.open(d.querySelector("form").elements.path.value, null);
    else { if (d.id === "dlg-wc") fillWC(d, d.dataset, "file" in d.dataset); openDialog(d); }
  });
  const hit = decodeURIComponent(location.hash.slice(1));
  if (hit) {
    const row = $$("tr[data-path]").find((r) => r.dataset.path === hit);
    if (row) { row.classList.add("hit"); row.scrollIntoView({ block: "center" }); setTimeout(() => row.classList.remove("hit"), 2000); }
  }
  connect();
})();
