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
    const f = e.target;
    if (!f.dataset || !f.dataset.confirm || f.dataset.confirmed) return;
    e.preventDefault();
    pending = { form: f, submitter: e.submitter };
    $("#cf-msg").textContent = f.dataset.confirm;
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
  const restoreLabel = (d) => rt(d.path ? "js.menu.restoreDir" : "js.menu.restoreFolder");
  const MENUS = {
    dir: (c) => [[rt("js.menu.open"), "open", "open"], [rt("js.menu.restoreAt"), "restore", "restore-at"]],
    file: () => [[rt("js.menu.versions"), "history", "versions"], [rt("js.menu.restoreAt"), "restore", "restore-at"]],
    gone: () => [[rt("js.menu.restoreLast"), "restore", "restore-last"], [rt("js.menu.versions"), "history", "versions"]],
    conflict: (c) => [[rt("js.menu.versions"), "history", "versions"], c.row.dataset.orig ? [rt("js.menu.versionsOrig"), "history", "versions-orig"] : null, [rt("js.menu.restoreAt"), "restore", "restore-at"]],
    unsynced: () => [[rt("js.menu.detail"), "info", "detail"]],
    blank: (c) => [[restoreLabel(c.row.dataset), "restore", "restore-dir"]],
    "folder-head": (c) => {
      const n = +c.row.dataset.maps;
      return [[restoreLabel(c.row.dataset), "restore", "restore-dir"],
        [rt("js.menu.delFolder"), "trash", "del-folder", { danger: true, disabled: n > 0, hint: n > 0 ? rt("js.menu.delFolderHint", { n }) : "" }]];
    },
    "map-row": () => [[rt("js.menu.editMap"), "edit", "edit-map"], [rt("js.menu.delMap"), "trash", "del-map", { danger: true }]],
    "dev-head": () => [[rt("js.menu.revoke"), "trash", "revoke", { danger: true }]],
  };
  const ACTIONS = {
    open: (c) => { location.href = $("a.nm", c.row).href; },
    versions: (c) => goto({ file: c.row.dataset.path }),
    "versions-orig": (c) => goto({ file: c.row.dataset.orig }),
    "restore-at": (c) => openRestore(c.row.dataset.path, c.row.dataset.kind === "dir"),
    "restore-dir": (c) => openRestore(c.row.dataset.path || "", !!c.row.dataset.path),
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
    "del-map": (c) => { const f = $("#f-del-map"); f.elements.folder.value = c.row.dataset.folder; f.requestSubmit(); },
    revoke: () => $("#f-revoke").requestSubmit(),
    "del-folder": () => $("#f-del-folder").requestSubmit(),
  };

  // openMenu shows the menu of a row (or of a button or an empty area) at a point of the viewport.
  function openMenu(row, kind, x, y, from) {
    const items = (MENUS[kind] || (() => []))({ row }).filter(Boolean);
    if (!items.length) return;
    menu.replaceChildren();
    if (row.dataset.path && /^(dir|file|gone|conflict|unsynced)$/.test(kind)) menu.append(el("div", { class: "mh" }, row.dataset.path + (kind === "dir" ? "/" : "")));
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
  async function refresh() {
    if (busy()) { dirty = true; return; }
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

  // ---- restore to a point in time: a time axis with a draggable line, and a preview (design 4.6)
  // The axis runs from the oldest state a restore can reach (left) to now (right). The server sends its ticks: the ends
  // of the minutes with a change in the scope. Dragging only moves the line and changes some text; the preview is asked
  // for when the state under the line changes and the line rests. Only the newest request counts, an older one is aborted.
  // Every text this code writes is in the dictionary (rt, above), with {placeholders}.

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
    const LABEL_GAP = 8;          // free space after a date label
    const STEP_WAIT = 300;        // keys and step buttons: ask for the preview once they rest this long
    const TYPE_WAIT = 400;        // typing: the same
    const PAGE = 10;              // PageUp / PageDown move this many changes
    const BASE = 36, AXIS_H = 52; // the axis' baseline and height in the svg
    // The zoom of the last 48 hours (design 4.6, the user's open question Q1, option A): a span longer than recent / frac is
    // drawn in two parts, the last `recent` ms in the right `frac` of the width. Set ZOOM to null for a linear axis; the
    // `.recent` rect and the two-part branch of makeScale are all that belong to it.
    const ZOOM = { recent: 48 * H, frac: 0.4 };

    const LANG = document.documentElement.lang || "en";
    const zone = zoneName();
    const form = rd.querySelector("form");
    const input = form.elements.time, go = $("[data-restore-go]", rd), goText = $("span", go);
    const axis = $("[data-axis]", rd), svg = $("svg", axis), cur = $(".axis-cur", axis), shade = $(".axis-shade", axis), amsg = $(".axis-msg", axis);
    const rel = $("[data-rel]", rd), pv = $("[data-preview]", rd), steps = $$("[data-step]", rd), note2 = $("[data-note2]", rd);
    const bar = el("div", { class: "progress", hidden: "" });
    const keepDays = +rd.dataset.keepDays;
    // fmtT: the time as the box shows it; whole minutes (the ticks, the dragged line) without seconds, the two ends with them
    const fmtT = (ms) => fmt(ms, zone).slice(0, ms % MIN ? 19 : 16);
    let T = null;     // the ticks of the scope: {now, from, ticks} (null: none loaded, none to show, or failed)
    let S = null;     // the scale of the drawn axis
    let W = 0;        // its width
    let sel = null;   // the selected time (ms); null: the box holds no time
    let shown = null, asked = null, askedMs = 0, result = null, ctl = null, waitT = 0, typeT = 0;
    const nowMs = () => (T ? T.now * 1000 : Date.now());

    // ---- scale: linear, or two linear parts (see ZOOM)
    function makeScale(from, now, w) {
      const split = ZOOM && now - from > ZOOM.recent / ZOOM.frac ? Math.round(w * (1 - ZOOM.frac)) : 0;
      const segs = split ? [{ a: from, b: now - ZOOM.recent, x0: 0, x1: split }, { a: now - ZOOM.recent, b: now, x0: split, x1: w }]
        : [{ a: from, b: now, x0: 0, x1: w }];
      const last = segs[segs.length - 1];
      return {
        segs,
        x: (ms) => { const s = segs.find((g) => ms <= g.b) || last; return s.x0 + (ms - s.a) / (s.b - s.a) * (s.x1 - s.x0); },
        t: (x) => { const s = segs.find((g) => x <= g.x1) || last; return s.a + (x - s.x0) / (s.x1 - s.x0) * (s.b - s.a); },
      };
    }
    // tick levels, aligned to the calendar of the zone: every h hours, days of the month 1, 1+d, ..., months m
    const LEVELS = [1, 2, 3, 6, 12].map((h) => ({ h, ms: h * H }))
      .concat([1, 2, 5, 10, 15].map((d) => ({ d, ms: d * D })), [1, 3, 6, 12].map((m) => ({ m, ms: m * 30.44 * D })));
    const DAY = LEVELS[5];
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
        for (let h = 0; h < 24; h += lv.h) { const x = ms + h * H; if (x > b) break; if (x >= a) out.push(x); } // DST days: approximate
      }
    }
    const ctx = document.createElement("canvas").getContext("2d");
    let font = "";
    const textW = (s, bold) => {
      if (!font) font = "11px " + getComputedStyle(document.body).getPropertyValue("--sans");
      ctx.font = (bold ? "600 " : "") + font;
      return ctx.measureText(s).width;
    };
    const labelFmts = {};
    const labelFmt = (lv) => labelFmts[!!lv.m] || (labelFmts[!!lv.m] = new Intl.DateTimeFormat(LANG,
      lv.m ? { timeZone: zone, year: "numeric", month: "short" } : { timeZone: zone, month: "short", day: "numeric" }));
    const sampleW = {};
    const labelW = (lv) => sampleW[!!lv.m] ?? (sampleW[!!lv.m] = textW(labelFmt(lv).format(Date.UTC(2026, 11, 28, 12))));
    function levelsFor(seg) {
      const k = (seg.x1 - seg.x0) / (seg.b - seg.a), sp = (lv) => lv.ms * k, top = LEVELS[LEVELS.length - 1];
      const minor = LEVELS.find((lv) => sp(lv) >= MINOR_PX) || top;
      const label = LEVELS.find((lv) => lv.ms >= D && sp(lv) >= labelW(lv) + LABEL_GAP) || top;
      const tall = minor.ms < D ? DAY : label;
      return { short: minor.ms < tall.ms ? minor : null, tall, label };
    }
    const px = (x) => Math.round(x) + 0.5;
    const esc = (s) => s.replace(/&/g, "&amp;").replace(/</g, "&lt;");
    function render() {
      W = axis.clientWidth;
      if (!T || !W) { svg.innerHTML = ""; S = null; return; }
      S = makeScale(T.from * 1000, T.now * 1000, W);
      svg.setAttribute("viewBox", `0 0 ${W} ${AXIS_H}`);
      let out = "", tall = "", short = "", marks = "";
      const labels = [];
      if (S.segs.length > 1) out += `<rect class="recent" x="${S.segs[1].x0}" y="0" width="${W - S.segs[1].x0}" height="${BASE}"/>`;
      for (const g of S.segs) {
        const L = levelsFor(g), tt = ticksOf(L.tall, g.a, g.b), ts = new Set(tt);
        tt.forEach((ms) => { tall += `M${px(S.x(ms))} 0V${BASE}`; });
        if (L.short) ticksOf(L.short, g.a, g.b).forEach((ms) => { if (!ts.has(ms)) short += `M${px(S.x(ms))} ${BASE - 6}V${BASE}`; });
        const f = labelFmt(L.label);
        ticksOf(L.label, g.a, g.b).forEach((ms) => labels.push([S.x(ms), f.format(ms)]));
      }
      const col = new Uint8Array(W); // one mark per pixel column that holds a change
      for (let i = 1; i < T.ticks.length - 1; i++) {
        const c = Math.min(W - 1, Math.round(S.x(T.ticks[i] * 1000)));
        if (!col[c]) { col[c] = 1; marks += `M${c + 0.5} ${BASE + 3}v10`; }
      }
      out += `<path class="tk" d="${tall}"/><path class="tk s" d="${short}"/><path class="base" d="M0 ${BASE + 0.5}H${W}"/><path class="mk" d="${marks}"/>`;
      out += `<text class="lb now" x="${W - 5}" y="12" text-anchor="end">${esc(rt("js.now"))}</text>`;
      let limit = W - 5 - textW(rt("js.now"), true) - LABEL_GAP; // labels from right to left; one that would overlap is left out
      labels.sort((a, b) => b[0] - a[0]);
      for (const [x, s] of labels) {
        if (x + 3 + textW(s) > limit) continue;
        out += `<text class="lb" x="${Math.round(x) + 3}" y="12">${esc(s)}</text>`;
        limit = x - 4;
      }
      svg.innerHTML = out;
      place();
    }
    function place() {
      cur.hidden = shade.hidden = sel === null;
      if (!S || sel === null) return;
      const x = S.x(Math.min(T.now * 1000, Math.max(T.from * 1000, sel)));
      cur.style.transform = shade.style.transform = `translateX(${x}px)`;
    }

    // ---- selection: the time, its state key, and whether the preview on the screen still belongs to it
    // idx: the last tick at or before ms (in ms); -1 before the first. From the last change to now nothing differs: one state.
    function idx(ms) {
      const k = T.ticks, s = ms / 1000;
      if (s < k[0]) return -1;
      let lo = 0, hi = k.length - 1;
      while (lo < hi) { const mid = (lo + hi + 1) >> 1; if (k[mid] <= s) lo = mid; else hi = mid - 1; }
      return Math.min(lo, k.length - 2);
    }
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
    // setSel shows the time ms everywhere: the box (unless the person is typing in it), the distance from now, the line.
    // It sends nothing: commit asks for the preview.
    function setSel(ms, typed) {
      sel = ms;
      if (!typed && ms !== null) input.value = fmtT(ms);
      rel.textContent = ms === null ? "" : relText(ms);
      place();
      if (T && ms !== null) {
        axis.setAttribute("aria-valuenow", Math.round(ms / 1000));
        axis.setAttribute("aria-valuetext", fmtT(ms) + ", " + relText(ms));
      }
      stale();
    }
    function stale() {
      const s = !(result && result.fail) && keyOf(sel) !== shown;
      pv.classList.toggle("stale", s);
      go.disabled = s || !result || !result.n;
    }
    function abort() { if (ctl) { ctl.abort(); ctl = null; } }
    // commit asks for the preview of the selected time unless the screen shows it or it is already asked for.
    function commit() {
      clearTimeout(waitT);
      const key = keyOf(sel);
      if (key === asked) return;
      abort();
      if (key === shown) { asked = null; bar.hidden = true; return; }
      asked = key;
      askedMs = sel;
      if (key === "bad") { show(key, { bad: true }); return; }
      bar.hidden = false;
      const ms = sel, c = ctl = new AbortController();
      const q = new URLSearchParams({ path: form.elements.path.value, time: utc(ms) });
      fetch(`${location.pathname}/restore-preview?${q}`, { credentials: "same-origin", signal: c.signal })
        .then((r) => r.ok ? r.text() : Promise.reject(r.status))
        .then((html) => {
          if (c !== ctl) return;
          const doc = new DOMParser().parseFromString(html, "text/html"), cnt = $("[data-count]", doc);
          show(keyOf(ms), { ms, n: cnt ? +cnt.dataset.count : 0, nodes: Array.from(doc.body.childNodes).map((n) => document.importNode(n, true)) });
        })
        .catch((e) => { if (c === ctl && !(e && e.name === "AbortError")) show(null, { fail: true }); });
    }
    function show(key, res) {
      shown = key; asked = null; result = res; ctl = null;
      bar.hidden = true;
      const r = res;
      let nodes = r.nodes;
      if (!nodes) { // a result of this script: the box holds no time, or the preview did not load
        nodes = [el("div", { class: "preview-h" + (r.bad ? " warn" : ""), "aria-live": "polite" }, icon(r.bad ? "warn" : "history"),
          el("span", {}, r.bad ? rt("js.bad", { ex: "2026-11-03 22:48:27" }) : rt("js.fail")))];
      } else if (!nodes.some((n) => n.classList && n.classList.contains("first-n")) && r.ms < nowMs() - keepDays * D) {
        nodes.splice(1, 0, el("div", { class: "first-n" }, icon("info"), el("span", {}, rt("js.old")))); // after the head line
      }
      pv.replaceChildren(bar, ...nodes);
      renderTimes(pv);
      goText.textContent = r.n ? rt("js.go", { n: r.n }) : r.bad || r.fail ? rt("common.restore") : rt("js.go0");
      stale();
    }

    // ---- input: pointer on the axis, keys on the axis, the step buttons, typing
    let drag = null, raf = 0, lastX = 0;
    const atX = (x) => Math.min(T.now * 1000, Math.max(T.from * 1000, Math.round(S.t(Math.max(0, Math.min(W, x))) / MIN) * MIN));
    axis.addEventListener("pointerdown", (e) => {
      if (!T || !S || e.button > 0) return;
      const r = axis.getBoundingClientRect(); // read once: no layout reads while the line moves
      drag = { left: r.left + 1, start: sel, moved: false, touch: e.pointerType === "touch" };
      axis.setPointerCapture(e.pointerId);
      axis.focus({ preventScroll: true });
      if (!drag.touch) setSel(atX(e.clientX - drag.left)); // a touch may still turn into a scroll: wait for a move
      e.preventDefault();
    });
    axis.addEventListener("pointermove", (e) => {
      if (!drag) return;
      drag.moved = true;
      lastX = e.clientX - drag.left;
      if (!raf) raf = requestAnimationFrame(() => { raf = 0; if (drag) setSel(atX(lastX)); });
    });
    axis.addEventListener("pointerup", (e) => {
      if (!drag) return;
      if (!drag.moved) setSel(atX(e.clientX - drag.left)); // a tap puts the line there
      drag = null;
      commit();
    });
    axis.addEventListener("pointercancel", () => { if (drag) { setSel(drag.start); drag = null; } }); // it was a scroll
    function stepTo(j) {
      const n = T.ticks.length - 1;
      setSel(j >= n - 1 ? T.now * 1000 : T.ticks[Math.max(0, j)] * 1000);
      clearTimeout(waitT);
      waitT = setTimeout(commit, STEP_WAIT);
    }
    const here = () => idx(sel === null ? T.now * 1000 : sel);
    axis.addEventListener("keydown", (e) => {
      if (!T) return;
      const i = here(), n = T.ticks.length - 1;
      const j = { ArrowLeft: i - 1, ArrowDown: i - 1, ArrowRight: i + 1, ArrowUp: i + 1, PageDown: i - PAGE, PageUp: i + PAGE, Home: 0, End: n }[e.key];
      if (j === undefined) return;
      e.preventDefault();
      stepTo(j);
    });
    steps.forEach((b) => b.addEventListener("click", () => { if (T) stepTo(here() + +b.dataset.step); }));
    input.addEventListener("input", () => {
      clearTimeout(typeT);
      typeT = setTimeout(() => { setSel(parseLocal(input.value, zone), true); commit(); }, TYPE_WAIT);
    });
    new ResizeObserver(() => { if (axis.clientWidth !== W) render(); }).observe(axis);
    form.addEventListener("submit", (e) => { // the server reads UTC: send the time the person typed as UTC
      const ms = parseLocal(input.value, zone);
      if (ms === null) { e.preventDefault(); setSel(null, true); commit(); return; }
      input.value = utc(ms);
    });
    rd.addEventListener("close", () => { abort(); clearTimeout(waitT); clearTimeout(typeT); });

    // ---- the note under the preview: time zone, retention, and the earliest point
    function fillNote() {
      const [a, b] = rt("js.note2", { tz: zone, how: rt(body.dataset.tz ? "js.fixed" : "js.follow") }).split("{link}");
      const link = el("a", { href: "/settings" }, rt("nav.settings"));
      note2.replaceChildren(a, link, b, T ? rt("js.note3", { d: keepDays, from: fmtT(T.from * 1000) }) : rt("js.note3b", { d: keepDays }));
    }

    // ---- opening: the preview of the time in the box at once, the axis when its ticks arrive
    async function loadTicks(path) {
      let t = null, failed = false;
      try {
        const r = await fetch(`${location.pathname}/restore-ticks?` + new URLSearchParams({ path }), { credentials: "same-origin" });
        if (!r.ok) failed = true; else t = await r.json();
      } catch (e) { failed = true; }
      if (form.elements.path.value !== path || !rd.open) return;
      if (t && !t.none && t.ticks.length >= 2) T = t;
      if (T) { // what was asked for or shown by the minute now has a state: the same state is not asked for again
        if (shown !== null && result && result.ms !== undefined) shown = idx(result.ms);
        if (asked !== null && asked !== "bad") asked = idx(askedMs);
      }
      axis.classList.toggle("off", !T);
      axis.tabIndex = T ? 0 : -1;
      steps.forEach((b) => { b.disabled = !T; });
      if (T) {
        axis.setAttribute("aria-valuemin", T.from);
        axis.setAttribute("aria-valuemax", T.now);
      } else {
        amsg.textContent = rt(failed ? "js.axisFail" : "js.axisNone");
        amsg.hidden = false;
      }
      fillNote();
      render();
      setSel(sel, true); // the distance from now and the state under the line follow the server's clock
      commit();
    }
    // open: path is the scope; isDir says how to write it (null: the page wrote it already)
    function open(path, isDir) {
      form.elements.path.value = path;
      if (isDir !== null) $('[data-fill="scope"]', rd).textContent = rd.dataset.folderName + (path ? " / " + path + (isDir ? "/" : "") : "");
      if (!input.value) input.value = fmtT(Math.floor(Date.now() / MIN) * MIN - H);
      abort();
      T = S = null; shown = asked = result = null;
      axis.hidden = false;
      axis.classList.add("off");
      amsg.hidden = true;
      steps.forEach((b) => { b.hidden = false; b.disabled = true; });
      pv.hidden = false;
      pv.replaceChildren(bar);
      fillNote();
      render();
      if (!rd.open) rd.showModal();
      setSel(parseLocal(input.value, zone), true);
      commit();
      loadTicks(path);
    }
    return { open };
  }
  const restore = $("#dlg-restore") && restoreDialog($("#dlg-restore"));
  function openRestore(path, isDir) { restore.open(path, isDir); }

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
          const mine = ++asking, text = ta.value;
          go.disabled = true;
          let html;
          try {
            const r = await fetch(form.getAttribute("action") + "/preview", { method: "POST", body: new URLSearchParams({ rules: text }), credentials: "same-origin" });
            html = r.ok ? await r.text() : null;
          } catch (e) { html = null; }
          if (mine !== asking) return;
          if (html === null) { // the rules are invalid or the server is out of reach: the errors list says which
            hitsBox.replaceChildren(el("div", { class: "err-msg" }, rt("js.previewFail")));
            shown = null;
          } else {
            const doc = new DOMParser().parseFromString(html, "text/html");
            hitsBox.replaceChildren(...Array.from(doc.body.childNodes).map((n) => document.importNode(n, true)));
            shown = text;
          }
          label();
        },
        needed: () => shown !== ta.value && ta.value !== saved0,
      };
      return self;
    })();
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
  $$("dialog[data-autoopen]").forEach((d) => { if (restore && d.id === "dlg-restore") restore.open(d.querySelector("form").elements.path.value, null); else openDialog(d); });
  const hit = decodeURIComponent(location.hash.slice(1));
  if (hit) {
    const row = $$("tr[data-path]").find((r) => r.dataset.path === hit);
    if (row) { row.classList.add("hit"); row.scrollIntoView({ block: "center" }); setTimeout(() => row.classList.remove("hit"), 2000); }
  }
  connect();
})();
