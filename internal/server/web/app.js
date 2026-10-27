/* Coop Loader UI. No framework, no build step: the Go binary serves these files. */
(() => {
  "use strict";

  const params = new URLSearchParams(location.search);
  const token = params.get("token") || "";
  if (token) history.replaceState(null, "", location.pathname);

  const $ = (sel) => document.querySelector(sel);
  const el = (tag, props = {}, ...kids) => {
    const node = document.createElement(tag);
    for (const [k, v] of Object.entries(props)) {
      if (k === "class") node.className = v;
      else if (k === "text") node.textContent = v;
      else if (k.startsWith("on") && typeof v === "function") node.addEventListener(k.slice(2), v);
      else if (v !== undefined && v !== null && v !== false) node.setAttribute(k, v === true ? "" : String(v));
    }
    for (const kid of kids.flat()) {
      if (kid === null || kid === undefined || kid === false) continue;
      node.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
    }
    return node;
  };

  const state = {
    status: null,
    mods: [],
    saves: null,
    profiles: null,
    filter: "",
    tab: "mods",
    session: null,
    sessionKey: "",
    sessionBusy: false,
    sessionLaunching: false,
    sessionTried: "",
    sessionLaunchedFor: "",
    sessionLaunchedAt: 0,
    lastRunning: false,
    lastSessionGameIn: false,
    sessionAuto: true,
    host: { slot: 1, players: 4 },
    hostSlotLoaded: false,
    joinDraft: "",
    browse: {
      q: "",
      sort: "downloads",
      category: 0,
      page: 1,
      pages: 1,
      items: [],
      categories: [],
      loading: false,
      loaded: false,
      error: null,
      fromCache: false,
    },
  };

  // ---------- API ----------

  async function api(path, options = {}) {
    const headers = Object.assign({}, options.headers, { "X-Token": token });
    if (typeof options.body === "string") headers["Content-Type"] = "application/json";
    const res = await fetch(path, { ...options, headers });
    const text = await res.text();
    let data = null;
    try { data = text ? JSON.parse(text) : null; } catch { /* non-JSON */ }
    if (!res.ok) {
      const err = new Error((data && data.error) || res.statusText || "Request failed");
      err.code = data && data.code;
      throw err;
    }
    return data;
  }

  const postJSON = (path, body) =>
    api(path, { method: "POST", body: JSON.stringify(body) });

  async function guard(fn) {
    try {
      await fn();
    } catch (err) {
      toast(err.message || String(err), "bad");
    }
  }

  // ---------- toast ----------

  let toastTimer = null;
  function toast(message, kind = "") {
    const node = $("#toast");
    node.textContent = message;
    node.className = "toast " + kind;
    node.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { node.hidden = true; }, kind === "bad" ? 7000 : 3500);
  }

  // ---------- status ----------

  async function refreshStatus() {
    state.status = await api("/api/status");
    renderStatus();
  }

  function renderStatus() {
    const st = state.status;
    if (!st) return;

    const chip = $("#game-chip");
    chip.className = "chip";
    if (st.running) {
      chip.textContent = "Game running";
      chip.classList.add("busy");
    } else if (st.gameFound) {
      chip.textContent = "Game found";
      chip.classList.add("ok");
    } else {
      chip.textContent = "Game not found";
      chip.classList.add("bad");
    }

    $("#version").textContent = "v" + st.version;
    $("#play").disabled = !st.gameFound;
    const runningChanged = state.lastRunning !== !!st.running;
    if (state.lastRunning && !st.running) {
      state.sessionLaunchedFor = "";
      state.sessionLaunchedAt = 0;
    }
    state.lastRunning = !!st.running;
    if (runningChanged && state.session) renderSession();
    if (state.sessionLaunchedFor && !st.running &&
        Date.now() - state.sessionLaunchedAt > 8000) {
      state.sessionLaunchedFor = "";
      if (state.session) renderSession();
    }
    $("#close-on-play").checked = !!st.closeOnPlay;

    if (!state.hostSlotLoaded && st.hostSlot >= 1 && st.hostSlot <= 4) {
      state.host.slot = st.hostSlot;
      state.hostSlotLoaded = true;
    }
    if (document.activeElement !== $("#game-path")) {
      $("#game-path").value = st.gamePath || "";
    }
    const gs = $("#game-state");
    if (st.gameFound) {
      gs.textContent = st.gamePath;
      gs.className = "state ok";
    } else {
      gs.textContent = "Not configured. Pick the folder that contains the game.";
      gs.className = "state bad";
    }

    $("#path-data").textContent = st.gameData;
    $("#path-config").textContent = st.configPath + (st.configFound ? "" : "  (missing)");
    $("#path-save").textContent = st.savePath + (st.saveFound ? "" : "  (missing)");
    $("#path-loader").textContent = st.loaderDir;
  }

  function mutationBlocked() {
    return state.status && state.status.running;
  }

  // ---------- mods ----------

  async function loadMods() {
    const data = await api("/api/mods");
    state.mods = data.mods || [];
    renderMods();
    if (state.browse.loaded) renderBrowse();
  }

  function renderMods() {
    const container = $("#mods-list");
    const q = state.filter.trim().toLowerCase();
    const list = state.mods.filter((m) => {
      if (!q) return true;
      return (m.name + " " + m.id + " " + m.description + " " + m.category)
        .toLowerCase()
        .includes(q);
    });

    container.replaceChildren(...list.map(modRow));
    if (!list.length) {
      container.append(el("p", { class: "boot" }, q ? "Nothing matches that filter." : "No mods installed yet."));
    }

    const on = state.mods.filter((m) => m.enabled).length;
    const total = state.mods.length;
    let hint = total
      ? `${total} installed · ${on} on. Changes apply the next time the game starts.`
      : "Install a .zip from the mod site or a .lua file you already have.";
    if (state.status && !state.status.configFound) {
      hint += " Start the game once so it creates its settings file.";
    }
    if (mutationBlocked()) {
      hint += " Close the game before changing mods.";
    }
    $("#mods-hint").textContent = hint;
  }

  function modRow(m) {
    const tags = [
      m.kind === "pack" ? el("span", { class: "tag pack" }, "DynOS pack") : null,
      m.origin === "builtin" ? el("span", { class: "tag builtin" }, "built in") : null,
      m.category ? el("span", { class: "tag" }, m.category) : null,
    ].filter(Boolean);

    const toggle = el("label", { class: "switch", title: m.enabled ? "Turn off" : "Turn on" },
      el("input", {
        type: "checkbox",
        checked: m.enabled,
        disabled: m.origin === "builtin" || mutationBlocked(),
        onchange: (ev) => toggleMod(m, ev.target.checked),
      }),
      el("span", { class: "knob", "aria-hidden": "true" })
    );

    const actions = [toggle];
    if (m.origin === "user") {
      actions.push(el("button", {
        class: "btn btn-ghost btn-danger",
        type: "button",
        onclick: () => removeMod(m),
      }, "Remove"));
    }

    return el("div", { class: "item" + (m.enabled ? "" : " off") },
      el("div", { class: "item-main" },
        el("div", { class: "item-title" }, el("strong", {}, m.name), ...tags),
        m.description ? el("p", { class: "item-desc" }, m.description) : null,
        el("div", { class: "item-id" }, m.site ? `${m.id} · site ${m.site.version}` : m.id)
      ),
      el("div", { class: "item-actions" }, ...actions)
    );
  }

  async function toggleMod(mod, on) {
    await guard(async () => {
      const res = await postJSON("/api/mods/toggle", { id: mod.id, kind: mod.kind, on });
      await loadMods();
      if (res.disabled && res.disabled.length) {
        toast("Turned off " + res.disabled.join(", ") + ": they cannot run together.", "ok");
      }
    });
  }

  async function removeMod(mod) {
    if (!confirm(`Remove "${mod.name}"?\n\nIt goes to removed-by-coop-loader, not the bin.`)) return;
    await guard(async () => {
      const res = await postJSON("/api/mods/remove", { id: mod.id, kind: mod.kind });
      await Promise.all([loadMods(), loadProfiles()]);
      toast("Moved to " + res.moved, "ok");
    });
  }

  async function uploadMod(file, allowCompiled = false) {
    const form = new FormData();
    form.append("file", file);
    form.append("allowCompiled", allowCompiled ? "true" : "false");
    const res = await api("/api/mods/install", { method: "POST", body: form });
    await loadMods();
    toast("Installed " + (res.installed || []).join(", ") + ".", "ok");
  }

  function onInstallPicked(ev) {
    const file = ev.target.files && ev.target.files[0];
    ev.target.value = "";
    if (!file) return;
    guard(async () => {
      try {
        await uploadMod(file, false);
      } catch (err) {
        if (err.code !== "compiled") throw err;
        const ok = confirm(
          "This mod contains compiled Lua, which cannot be reviewed.\n\n" +
          "Mods run code inside your game. Install it only if you trust the author."
        );
        if (!ok) return;
        await uploadMod(file, true);
      }
    });
  }


  // ---------- browse ----------

  const normVer = (v) => String(v || "").trim().replace(/^v/i, "").trim();
  const sameVer = (a, b) => normVer(a) === normVer(b);

  function recordFor(siteId) {
    for (const m of state.mods) {
      if (m.site && m.site.siteId === siteId) return m.site;
    }
    return null;
  }

  // Mods installed by hand have no site record. Match them by name so Browse
  // can say "you already have this" instead of offering a blind download.
  function nameKey(value) {
    return String(value || "")
      .toLowerCase()
      .replace(/\\#[0-9a-f]{6}(?:[0-9a-f]{2})?\\/g, " ")
      .replace(/\bv?\d+(?:\.\d+)+\b/g, " ")
      .replace(/[^a-z0-9]+/g, "");
  }

  function manualMatch(item) {
    const key = nameKey(item.title);
    if (!key) return null;
    for (const m of state.mods) {
      if (m.origin !== "user") continue;
      for (const candidate of [nameKey(m.name), nameKey(m.id)]) {
        if (!candidate) continue;
        if (candidate === key) return m;
        if (candidate.length >= 8 && key.length >= 8 &&
            (candidate.startsWith(key) || key.startsWith(candidate))) {
          return m;
        }
      }
    }
    return null;
  }

  function browseStatus(item) {
    const rec = recordFor(item.id);
    if (rec) return sameVer(rec.version, item.version) ? "installed" : "update";
    return manualMatch(item) ? "found" : "none";
  }

  async function loadCategories() {
    if (state.browse.categories.length) return;
    const data = await api("/api/catalog/categories");
    state.browse.categories = data.categories || [];
    const select = $("#browse-cat");
    for (const c of state.browse.categories) {
      select.append(el("option", { value: String(c.id) }, "  ".repeat(c.depth || 0) + c.title));
    }
  }

  async function loadBrowse(reset) {
    const b = state.browse;
    if (b.loading) return;
    if (reset) {
      b.page = 1;
      b.items = [];
      b.loaded = false;
    }
    const requestedPage = b.page;
    b.loading = true;
    b.error = null;
    renderBrowse();

    try {
      const params = new URLSearchParams({
        sort: b.sort,
        page: String(b.page),
        q: b.q,
      });
      if (b.category) params.set("category", String(b.category));
      const data = await api("/api/catalog/list?" + params.toString());
      b.items = reset ? data.items || [] : b.items.concat(data.items || []);
      b.pages = data.pages || 1;
      b.fromCache = !!data.fromCache;
      b.loaded = true;
    } catch (err) {
      if (!reset && b.page === requestedPage) {
        b.page = Math.max(1, b.page - 1);
      }
      b.error = err.message || String(err);
    } finally {
      b.loading = false;
      renderBrowse();
    }
  }

  function renderBrowse() {
    const b = state.browse;
    const list = $("#browse-list");
    if (!b.loaded && b.loading) {
      list.replaceChildren(el("p", { class: "boot" }, "Loading the mod site…"));
      $("#browse-more").hidden = true;
      return;
    }
    if (b.error) {
      list.replaceChildren(el("div", { class: "notice" }, b.error));
      $("#browse-more").hidden = true;
      return;
    }

    list.replaceChildren(...b.items.map(browseRow));
    if (!b.items.length && b.loaded) {
      list.append(el("p", { class: "boot" }, "No mods match that search."));
    }
    $("#browse-more").hidden = b.page >= b.pages || b.loading;
    $("#browse-hint").textContent = b.loaded
      ? `Page ${b.page} of ${b.pages}${b.fromCache ? " · cached" : ""}. Click a title to read the mod.`
      : "Mods come from mods.sm64coopdx.com. The loader asks before installing.";
  }

  function browseRow(item) {
    const status = browseStatus(item);
    const install = el("button", {
      class: "btn" + (status === "update" ? " btn-accent" : ""),
      type: "button",
      disabled: status === "installed",
    }, status === "installed" ? "Installed" : status === "update" ? "Update" : "Install");
    install.addEventListener("click", () => guard(() => installSite(item)));

    const icon = item.icon
      ? el("img", {
          class: "browse-icon",
          src: item.icon,
          alt: "",
          loading: "lazy",
          onerror: (ev) => { ev.target.style.visibility = "hidden"; },
        })
      : el("div", { class: "browse-icon" });

    return el("div", { class: "browse-item" },
      icon,
      el("div", { class: "browse-main" },
        el("div", { class: "browse-title" },
          el("button", { type: "button", onclick: () => guard(() => openDetail(item)) }, item.title),
          item.version ? el("span", { class: "tag" }, item.version) : null,
          status === "update" ? el("span", { class: "tag pack" }, "update") : null,
          status === "found" ? el("span", { class: "tag", title: "There is a mod with this name in your mods folder. Installing from here will track it." }, "found") : null
        ),
        el("div", { class: "browse-sub" }, [item.author, item.category].filter(Boolean).join(" · ")),
        item.tagline ? el("p", { class: "browse-tagline" }, item.tagline) : null,
        el("div", { class: "browse-stats" },
          item.rating ? el("span", {}, `★ ${Number(item.rating).toFixed(1)}${item.ratingCount ? ` (${item.ratingCount})` : ""}`) : null,
          item.downloads ? el("span", {}, `${Number(item.downloads).toLocaleString()} downloads`) : null
        )
      ),
      el("div", { class: "browse-action" }, install)
    );
  }

  async function openDetail(item) {
    const dialog = el("dialog", { class: "dialog detail-dialog" });
    const close = () => dialog.close();
    const title = el("h3", {}, item.title);
    const body = el("div", { class: "detail-body" }, el("p", { class: "boot" }, "Loading…"));

    dialog.append(
      el("div", { class: "detail-head" }, title, el("button", { class: "btn btn-ghost", type: "button", onclick: close }, "Close")),
      body
    );
    dialog.addEventListener("close", () => dialog.remove());
    document.body.append(dialog);
    dialog.showModal();

    let detail;
    try {
      detail = await api("/api/catalog/detail?url=" + encodeURIComponent(item.url));
    } catch (err) {
      body.replaceChildren(el("div", { class: "notice" }, err.message || "Could not load this mod’s page."));
      return;
    }
    title.textContent = detail.title || item.title;

    const status = browseStatus(detail);
    const install = el("button", {
      class: "btn" + (status === "update" ? " btn-accent" : ""),
      type: "button",
      disabled: status === "installed",
    }, status === "installed" ? "Installed" : status === "update" ? "Update" : "Install");
    install.addEventListener("click", () => guard(async () => {
      dialog.close();
      await installSite(detail);
    }));

    const meta = el("div", { class: "detail-meta" },
      detail.author ? el("span", {}, "by " + detail.author) : null,
      detail.category ? el("span", {}, detail.category) : null,
      detail.version ? el("span", {}, "v" + normVer(detail.version)) : null,
      detail.rating ? el("span", {}, "★ " + Number(detail.rating).toFixed(1)) : null,
      detail.downloads ? el("span", {}, Number(detail.downloads).toLocaleString() + " downloads") : null,
      status === "found" ? el("span", {}, "already in your mods folder") : null
    );

    const links = (detail.links && detail.links.length)
      ? el("div", { class: "detail-links" },
          el("span", { class: "caption" }, "Linked mods: "),
          ...detail.links.map((link) => el("button", {
            class: "btn btn-ghost",
            type: "button",
            onclick: () => guard(() => installSite(link)),
          }, link.title))
        )
      : null;

    const nodes = [meta];
    if (links) nodes.push(links);
    nodes.push(...renderBlocks(detail.blocks));
    nodes.push(el("div", { class: "detail-actions" },
      install,
      el("button", { class: "btn btn-ghost", type: "button", onclick: () => openExternal(detail.url) }, "Open page")
    ));
    body.replaceChildren(...nodes);
  }

  function renderBlocks(blocks) {
    const out = [];
    for (const b of blocks || []) {
      switch (b.kind) {
        case "image":
          out.push(el("img", { class: "detail-image", src: b.src, alt: "", loading: "lazy" }));
          break;
        case "list": {
          const list = el("ul");
          for (const item of b.items || []) list.append(el("li", {}, ...renderInline(item)));
          out.push(list);
          break;
        }
        case "quote":
          out.push(el("blockquote", {}, ...renderInline(b.inline)));
          break;
        case "h":
          out.push(el("h3", { class: "detail-h" }, ...renderInline(b.inline)));
          break;
        default:
          out.push(el("p", {}, ...renderInline(b.inline)));
      }
    }
    return out;
  }

  function renderInline(items) {
    const out = [];
    for (const run of items || []) {
      if (run.br) {
        out.push(el("br"));
        continue;
      }
      const text = run.text || "";
      let node = document.createTextNode(text);
      if (run.href) {
        node = el("a", {
          href: run.href,
          rel: "noreferrer noopener",
          onclick: (ev) => { ev.preventDefault(); openExternal(run.href); },
        }, text);
      }
      if (run.code) node = el("code", {}, node);
      if (run.bold) node = el("strong", {}, node);
      if (run.italic) node = el("em", {}, node);
      if (run.underline) node = el("u", {}, node);
      if (run.strike) node = el("s", {}, node);
      out.push(node);
    }
    return out;
  }

  async function installSite(item, opts = {}) {
    const body = {
      url: item.url,
      file: opts.file || "",
      allowCompiled: !!opts.allow,
    };
    let res;
    try {
      res = await postJSON("/api/catalog/install", body);
    } catch (err) {
      if (err.code === "compiled" && !opts.allow) {
        const ok = confirm(
          "This mod contains compiled Lua, which cannot be reviewed.\n\n" +
          "Mods run code inside your game. Install it only if you trust the author."
        );
        if (!ok) return;
        return installSite(item, { ...opts, allow: true });
      }
      throw err;
    }

    if (res.kind === "external") {
      openExternalDialog(res.url);
      return;
    }
    if (res.kind === "choose") {
      openChooseDialog(item, res.files || []);
      return;
    }
    if (res.kind === "installed") {
      await loadMods();
      renderBrowse();
      toast("Installed " + (res.installed || []).join(", ") + ".", "ok");
      if (res.requires && res.requires.length) openRequiresDialog(res.requires);
    }
  }

  function openChooseDialog(item, files) {
    const dialog = el("dialog", { class: "dialog" });
    const close = () => dialog.close();
    const list = el("div", { class: "copy-targets" });
    for (const f of files) {
      list.append(el("button", {
        class: "btn",
        type: "button",
        onclick: () => {
          dialog.close();
          guard(() => installSite(item, { file: f.url }));
        },
      }, f.name + (f.size ? ` (${f.size})` : "")));
    }
    dialog.append(
      el("h3", {}, "Pick a file"),
      el("p", { class: "hint" }, "This mod comes in more than one file."),
      list,
      el("div", { class: "dialog-actions" },
        el("button", { class: "btn", type: "button", onclick: close }, "Cancel")
      )
    );
    dialog.addEventListener("close", () => dialog.remove());
    document.body.append(dialog);
    dialog.showModal();
  }

  function openExternalDialog(url) {
    let host = "";
    try { host = new URL(url).hostname; } catch { /* ignore */ }
    const dialog = el("dialog", { class: "dialog" });
    const close = () => dialog.close();
    dialog.append(
      el("h3", {}, "Open on the author’s page"),
      el("p", { class: "hint" },
        `This mod is hosted on ${host || "another site"}, not on the mod site. ` +
        "The loader does not download from there. Open the page and get the file yourself."
      ),
      el("div", { class: "dialog-actions" },
        el("button", { class: "btn", type: "button", onclick: close }, "Cancel"),
        el("button", {
          class: "btn btn-accent",
          type: "button",
          onclick: () => { close(); openExternal(url); },
        }, "Open in browser")
      )
    );
    dialog.addEventListener("close", () => dialog.remove());
    document.body.append(dialog);
    dialog.showModal();
  }

  function openRequiresDialog(requires) {
    const dialog = el("dialog", { class: "dialog detail-dialog" });
    const close = () => dialog.close();
    const body = el("div", { class: "requires" });

    for (const req of requires) {
      const row = el("div", { class: "require" },
        el("div", { class: "require-head" }, "Needs ", el("code", {}, "_G." + req.global)),
        el("p", { class: "hint" }, "No enabled mod provides it, so this mod may not work yet.")
      );
      for (const provider of req.local || []) {
        row.append(el("button", {
          class: "btn",
          type: "button",
          onclick: () => guard(async () => {
            await postJSON("/api/mods/toggle", { id: provider.id, kind: "mod", on: true });
            await loadMods();
            row.remove();
            toast("Turned on " + provider.name + ".", "ok");
          }),
        }, "Enable " + provider.name));
      }
      for (const candidate of req.site || []) {
        row.append(el("button", {
          class: "btn btn-ghost",
          type: "button",
          onclick: () => guard(() => installSite(candidate)),
        }, "Install " + candidate.title));
      }
      body.append(row);
    }

    dialog.append(
      el("div", { class: "detail-head" },
        el("h3", {}, "This mod has requirements"),
        el("button", { class: "btn btn-ghost", type: "button", onclick: close }, "Close")
      ),
      body
    );
    dialog.addEventListener("close", () => dialog.remove());
    document.body.append(dialog);
    dialog.showModal();
  }

  async function openExternal(url) {
    await guard(() => postJSON("/api/open-external", { url }));
  }


  // ---------- play with friends ----------

  const NET_TERMS = 2;
  const RECENT_CODES = "recentJoinCodes";
  const AUTO_OPEN = "autoOpenGame";
  const CODE_ALPHABET = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";

  state.sessionAuto = (() => {
    try { return localStorage.getItem(AUTO_OPEN) !== "off"; } catch { return true; }
  })();

  function normalizeCode(raw) {
    const clean = String(raw || "")
      .toUpperCase()
      .replace(/[\s_-]/g, "")
      .replace(/O/g, "0")
      .replace(/[IL]/g, "1")
      .replace(/U/g, "V");
    if (!/^[0-9A-HJKMNP-TV-Z]{8}$/.test(clean)) return null;
    return clean.slice(0, 4) + "-" + clean.slice(4);
  }

  function recentCodes() {
    try {
      const list = JSON.parse(localStorage.getItem(RECENT_CODES) || "[]");
      return Array.isArray(list) ? list.filter((c) => normalizeCode(c)) : [];
    } catch { return []; }
  }

  function rememberCode(code) {
    try {
      const next = [code, ...recentCodes().filter((c) => c !== code)].slice(0, 4);
      localStorage.setItem(RECENT_CODES, JSON.stringify(next));
    } catch { /* storage disabled */ }
  }

  async function copyCode(code) {
    try {
      await navigator.clipboard.writeText(code);
      toast("Code copied.", "ok");
    } catch {
      toast("Could not copy the code. Select it and copy it by hand.", "bad");
    }
  }

  function ensureConsent() {
    const consent = state.status && state.status.netConsent;
    if (consent && consent.accepted && consent.termsVersion === NET_TERMS) {
      return Promise.resolve(true);
    }
    return new Promise((resolve) => {
      let answered = false;
      const dialog = el("dialog", { class: "dialog session-consent" });
      const finish = async (accepted) => {
        if (answered) return;
        answered = true;
        dialog.close();
        dialog.remove();
        if (accepted) {
          try {
            await postJSON("/api/settings/netconsent", { accepted: true, termsVersion: NET_TERMS });
            if (state.status) state.status.netConsent = { accepted: true, termsVersion: NET_TERMS };
          } catch (err) {
            toast(err.message || "Could not save that choice.", "bad");
            resolve(false);
            return;
          }
        }
        resolve(accepted);
      };
      dialog.append(
        el("h3", {}, "Play with friends using the connection helper"),
        el("p", {},
          "The connection helper links your game to your friend's through this app, so nobody needs a VPN, router settings or CoopNet."
        ),
        el("ul", { class: "consent-list" },
          el("li", {}, "You get a code, and your friend types it in."),
          el("li", {}, "The two apps find each other through public WebTorrent trackers and this app's relay on Cloudflare. What they exchange is locked with your code."),
          el("li", {}, "Your internet address is shared with those meeting points, the relay and the address-lookup services of Google and Cloudflare. Your game traffic only goes directly to your friend; the relay is used only when a direct route is impossible."),
          el("li", {}, "When you join, the host's game sends its active mods to yours, and your game runs their Lua. Join only people you trust.")
        ),
        el("p", { class: "hint" },
          "If you say no, you can still play online the usual way: CoopNet or Direct Connection in the game's own menu."
        ),
        el("div", { class: "dialog-actions" },
          el("button", { class: "btn", type: "button", onclick: () => finish(false) }, "Not now"),
          el("button", { class: "btn btn-accent", type: "button", onclick: () => finish(true) }, "Use the connection helper")
        )
      );
      dialog.addEventListener("cancel", (ev) => { ev.preventDefault(); finish(false); });
      dialog.addEventListener("close", () => {
        if (!answered) { answered = true; resolve(false); }
      });
      document.body.append(dialog);
      dialog.showModal();
    });
  }

  async function refreshSession(force) {
    let data;
    try {
      data = await api("/api/session/state");
    } catch {
      return;
    }
    const next = data.session || null;
    const key = next ? JSON.stringify(next) : "";
    const changed = key !== state.sessionKey;
    const nowIn = !!(next && next.gameIn);
    if (state.lastSessionGameIn && !nowIn) {
      // The game closed; the room stayed up and can open it again. Do not
      // clear sessionTried: auto-open happens once per room, not after every
      // game exit.
      state.sessionLaunchedFor = "";
      state.sessionLaunchedAt = 0;
    }
    state.lastSessionGameIn = nowIn;
    state.session = next;
    state.sessionKey = key;
    if (changed || force) {
      if (!next && state.tab !== "play") {
        // The panel is not visible, but the header pill must go away.
        renderSessionPill();
      } else {
        renderSession();
      }
    }
    maybeAutoLaunch();
    if (state.tab === "play") {
      const details = $("#session-log-card");
      if (details && details.open) refreshSessionLogs().catch(() => {});
    }
  }

  function renderSessionPill() {
    const pill = $("#session-pill");
    const s = state.session;
    if (!s) {
      pill.hidden = true;
      return;
    }
    pill.hidden = false;
    let text = "Session";
    if (s.mode === "host") text = `Hosting · ${s.code}`;
    else if (s.phase === "connected") text = "Playing with a friend";
    else if (s.phase === "failed") text = "Not connected";
    else text = "Connecting to a friend…";
    if (s.phase === "stopped") text = "Session stopped";
    pill.textContent = text;
  }

  async function refreshSessionLogs() {
    const data = await api("/api/session/logs");
    const lines = data.logs || [];
    $("#session-log").textContent = lines.length
      ? lines.slice(-400).join("\n")
      : "No log lines yet.";
  }

  async function startHost() {
    if (state.sessionBusy) return;
    if (!(await ensureConsent())) return;
    state.sessionBusy = true;
    renderSession();
    try {
      const data = await postJSON("/api/session/start", { mode: "host", code: "" });
      state.session = data.session;
      state.sessionTried = "";
      state.sessionLaunchedFor = "";
      state.sessionKey = JSON.stringify(data.session);
      renderSession();
      copyCode(data.session.code);
    } catch (err) {
      toast("Couldn't start hosting. " + (err.message || err), "bad");
    } finally {
      state.sessionBusy = false;
      renderSession();
    }
  }

  async function startJoin(raw) {
    const code = normalizeCode(raw);
    if (!code) {
      toast("That doesn't look like a code. It has 8 letters and numbers, like K7QD-9XPA.", "bad");
      return;
    }
    if (!(await ensureConsent())) return;
    state.sessionBusy = true;
    state.joinDraft = code;
    rememberCode(code);
    renderSession();
    try {
      const data = await postJSON("/api/session/start", { mode: "join", code });
      state.session = data.session;
      state.sessionTried = "";
      state.sessionLaunchedFor = "";
      state.sessionKey = JSON.stringify(data.session);
      renderSession();
    } catch (err) {
      toast("Couldn't start. " + (err.message || err), "bad");
    } finally {
      state.sessionBusy = false;
      renderSession();
    }
  }

  async function stopSession(quiet) {
    if (state.sessionBusy) return;
    state.sessionBusy = true;
    renderSession();
    const code = state.session && state.session.code;
    if (code) state.joinDraft = code;
    try {
      await postJSON("/api/session/stop", {});
    } catch (err) {
      toast(err.message || "Could not stop the session.", "bad");
    } finally {
      state.session = null;
      state.sessionKey = "";
      state.sessionTried = "";
      state.sessionLaunchedFor = "";
      state.lastSessionGameIn = false;
      state.sessionBusy = false;
      renderSession();
    }
    if (!quiet) toast("Session stopped.", "ok");
  }

  async function launchSession() {
    if (!state.session || state.sessionLaunching) return;
    state.sessionLaunching = true;
    renderSession();
    try {
      const data = await postJSON("/api/session/launch", {
        slot: state.host.slot,
        players: state.host.players,
      });
      state.session = data.session;
      state.sessionKey = JSON.stringify(data.session);
      state.sessionLaunchedFor = state.session.code || "";
      state.sessionLaunchedAt = Date.now();
      toast("Opening the game…", "ok");
    } catch (err) {
      toast(err.message || "Could not start the game.", "bad");
    } finally {
      state.sessionLaunching = false;
      renderSession();
    }
  }

  function maybeAutoLaunch() {
    const s = state.session;
    if (!s || !state.sessionAuto || state.sessionLaunching || s.gameIn) return;
    if (state.status && state.status.running) return;
    if (s.phase === "failed" || s.phase === "stopped") return;
    const friend = s.friends && s.friends[0];
    const ready = s.mode === "host"
      ? !!(s.friends && s.friends.length)
      : (s.phase === "connected" && friend && !friend.reconnecting && friend.game !== false);
    if (!ready) return;
    if (state.sessionTried === s.code) return;
    state.sessionTried = s.code;
    launchSession();
  }

  function sessionStep(n, title, ...kids) {
    return el("section", { class: "step" },
      el("div", { class: "step-head" },
        el("span", { class: "step-num", "aria-hidden": "true" }, String(n)),
        el("h3", { class: "step-title" }, title)
      ),
      ...kids
    );
  }

  function hostStatus(s) {
    if (s.phase === "failed") return s.message || "Couldn't start hosting.";
    if (s.trackers === 0) return "Getting online…";
    if (!s.friends.length) return "Online. Waiting for friends to join…";
    return s.friends.length === 1
      ? "1 friend is playing."
      : `${s.friends.length} friends are playing.`;
  }

  function friendRows(friends) {
    if (!friends || !friends.length) {
      return el("p", { class: "caption" }, "Nobody yet. They join with your code.");
    }
    return el("div", { class: "session-friends" }, ...friends.map((f) => {
      const route = f.reconnecting
        ? "reconnecting"
        : f.route === "relay"
          ? "relay"
          : f.route === "direct"
            ? "direct"
            : "route unknown";
      return el("div", { class: "session-friend" },
        el("span", { class: "session-friend-id" }, String(f.id).slice(0, 6)),
        el("span", { class: "tag " + (f.reconnecting ? "pack" : "") }, route),
        el("span", { class: "caption" },
          f.game === true ? "game in" : f.game === false ? "game not open" : "waiting for their game"
        )
      );
    }));
  }

  function autoChoice() {
    return el("label", { class: "field-label" }, "Game opens",
      el("select", {
        class: "input",
        onchange: (ev) => {
          state.sessionAuto = ev.target.value === "auto";
          try { localStorage.setItem(AUTO_OPEN, state.sessionAuto ? "on" : "off"); } catch { /* ignore */ }
        },
      },
        el("option", { value: "auto", selected: state.sessionAuto ? "selected" : null }, "By itself"),
        el("option", { value: "manual", selected: state.sessionAuto ? null : "selected" }, "When I choose")
      )
    );
  }

  function introView() {
    const hostSlot = el("select", {
      class: "input",
      onchange: (ev) => { state.host.slot = Number(ev.target.value); },
    }, ...[1, 2, 3, 4].map((n) => el("option", { value: String(n), selected: state.host.slot === n ? "selected" : null }, String(n))));

    const hostPlayers = el("select", {
      class: "input",
      onchange: (ev) => { state.host.players = Number(ev.target.value); },
    }, ...[2, 4, 8, 16].map((n) => el("option", { value: String(n), selected: state.host.players === n ? "selected" : null }, String(n))));

    const hostCard = el("div", { class: "session-card" },
      el("h3", {}, "Host a game"),
      el("p", {}, "You get a code. Your friends type it in and their games join yours."),
      el("div", { class: "session-options" },
        el("label", { class: "field-label" }, "Save file", hostSlot),
        el("label", { class: "field-label" }, "Players", hostPlayers),
        autoChoice()
      ),
      el("div", { class: "session-actions" },
        el("button", {
          class: "btn btn-accent",
          type: "button",
          disabled: state.sessionBusy,
          onclick: () => guard(startHost),
        }, state.sessionBusy ? "Starting…" : "Host a game")
      ),
      el("p", { class: "hint" }, "Your friend's game downloads your active mods when they join. The game opens on port 7777.")
    );

    const input = el("input", {
      class: "input join-input",
      type: "text",
      maxlength: "12",
      spellcheck: "false",
      autocomplete: "off",
      placeholder: "ABCD-1234",
      "aria-label": "Code from your friend",
      value: state.joinDraft || "",
      oninput: (ev) => { state.joinDraft = ev.target.value; },
      onkeydown: (ev) => {
        if (ev.key === "Enter") { ev.preventDefault(); guard(() => startJoin(ev.target.value)); }
      },
    });

    const recents = recentCodes();
    const recentRow = recents.length
      ? el("div", { class: "recent-codes" },
          el("span", { class: "caption" }, "Recent:"),
          ...recents.map((code) => el("button", {
            class: "btn btn-sm",
            type: "button",
            onclick: () => guard(() => startJoin(code)),
          }, code))
        )
      : null;

    const joinCard = el("div", { class: "session-card" },
      el("h3", {}, "Join a game"),
      el("p", {}, "Type the code your friend got when they pressed Host a game."),
      input,
      recentRow,
      el("div", { class: "session-actions" },
        el("button", {
          class: "btn btn-accent",
          type: "button",
          disabled: state.sessionBusy,
          onclick: () => guard(() => startJoin(input.value)),
        }, state.sessionBusy ? "Starting…" : "Join")
      ),
      el("p", { class: "hint" }, "The code is the key to the room. Treat it like a password.")
    );

    return [el("div", { class: "session-grid" }, hostCard, joinCard)];
  }

  function hostView(s) {
    const copy = el("button", { class: "btn", type: "button", onclick: () => copyCode(s.code) }, "Copy code");
    const launched = state.sessionLaunchedFor === s.code;
    const open = el("button", {
      class: "btn btn-accent",
      type: "button",
      disabled: s.gameIn || launched || state.sessionLaunching || !!(state.status && state.status.running),
      onclick: () => guard(launchSession),
    }, s.gameIn ? "Game is open" : launched ? "Waiting for your game…" : (state.status && state.status.running) ? "Game is running" : state.sessionLaunching ? "Opening…" : "Open the game");

    const runningNote = !s.gameIn && state.status && state.status.running
      ? el("p", { class: "hint" }, "The game is already open without the room. Close it, then open it from here.")
      : null;

    const options = s.gameIn ? null : el("div", { class: "session-options" },
      el("label", { class: "field-label" }, "Save file",
        el("select", {
          class: "input",
          onchange: (ev) => { state.host.slot = Number(ev.target.value); },
        }, ...[1, 2, 3, 4].map((n) => el("option", { value: String(n), selected: state.host.slot === n ? "selected" : null }, String(n))))
      ),
      el("label", { class: "field-label" }, "Players",
        el("select", {
          class: "input",
          onchange: (ev) => { state.host.players = Number(ev.target.value); },
        }, ...[2, 4, 8, 16].map((n) => el("option", { value: String(n), selected: state.host.players === n ? "selected" : null }, String(n))))
      ),
      autoChoice()
    );

    return [
      el("div", { class: "session-head" },
        el("div", {},
          el("p", { class: "session-kicker" }, "Hosting"),
          el("code", { class: "session-code", tabindex: "0" }, s.code)
        ),
        el("div", { class: "session-head-actions" }, copy)
      ),
      sessionStep(1, "Send this code to your friends",
        el("p", { class: "status-line" }, hostStatus(s))
      ),
      sessionStep(2, "Open the game", options,
        el("div", { class: "session-actions" }, open),
        runningNote
      ),
      sessionStep(3, "Your friends", friendRows(s.friends)),
      el("div", { class: "session-actions" },
        el("button", {
          class: "btn btn-danger",
          type: "button",
          disabled: state.sessionBusy,
          onclick: () => { if (confirm("Stop hosting? Your friends leave the game.")) guard(() => stopSession()); },
        }, "Stop hosting")
      ),
      el("p", { class: "hint" }, s.trackers > 0
        ? `${s.trackers} meeting point${s.trackers === 1 ? "" : "s"} online.`
        : "Trying to reach the meeting points…")
    ];
  }

  function joinStatus(s) {
    if (s.phase === "failed") return s.message || "Couldn't connect.";
    if (s.phase === "connected") {
      const f = s.friends && s.friends[0];
      if (f && f.reconnecting) return "The connection dropped. Reconnecting… Your place in the game is kept for a minute.";
      if (f && f.route === "relay") return "Found your friend, through the relay.";
      return "Found your friend.";
    }
    if (s.trackers === 0) return "Getting online…";
    return "Looking for your friend…";
  }

  function joinView(s) {
    const f = s.friends && s.friends[0];
    const ready = s.phase === "connected" && f && !f.reconnecting && f.game !== false;
    const launched = state.sessionLaunchedFor === s.code;
    const open = el("button", {
      class: "btn btn-accent",
      type: "button",
      disabled: !ready || s.gameIn || launched || state.sessionLaunching || !!(state.status && state.status.running),
      onclick: () => guard(launchSession),
    }, s.gameIn ? "Game is open" : launched ? "Waiting for your game…" : (state.status && state.status.running) ? "Game is running" : state.sessionLaunching ? "Opening…" : "Open the game");

    const gameLine = s.gameIn
      ? "Your game is in."
      : f && f.game === false
        ? "Your friend hasn't opened their game yet."
        : ready
          ? "Your friend is there. Open the game when you're ready."
          : "You can open it once your friend is found.";

    const actions = s.phase === "failed"
      ? [el("button", {
          class: "btn btn-accent",
          type: "button",
          onclick: () => guard(async () => {
            state.joinDraft = s.code;
            await stopSession(true);
          }),
        }, "Try again")]
      : [el("button", {
          class: "btn",
          type: "button",
          disabled: state.sessionBusy,
          onclick: () => guard(() => stopSession()),
        }, "Leave")];

    return [
      el("div", { class: "session-head" },
        el("div", {},
          el("p", { class: "session-kicker" }, "Joining"),
          el("code", { class: "session-code", tabindex: "0" }, s.code)
        )
      ),
      sessionStep(1, "Your friend's code",
        el("p", { class: "status-line" }, joinStatus(s))
      ),
      sessionStep(2, "Open the game",
        el("p", { class: "caption" }, gameLine),
        el("p", { class: "hint" },
          "Before you enter, your game downloads any of your friend's active mods that you don't have. The first join can take a while."
        ),
        autoChoice(),
        el("div", { class: "session-actions" }, open),
        !s.gameIn && state.status && state.status.running
          ? el("p", { class: "hint" }, "The game is already open without the room. Close it, then open it from here.")
          : null
      ),
      el("div", { class: "session-actions" }, ...actions),
      el("p", { class: "hint" }, s.trackers > 0
        ? `${s.trackers} meeting point${s.trackers === 1 ? "" : "s"} online.`
        : "Trying to reach the meeting points…")
    ];
  }

  function renderSession() {
    const panel = $("#session-panel");
    const s = state.session;
    if (s) {
      panel.replaceChildren(...(s.mode === "host" ? hostView(s) : joinView(s)));
    } else {
      panel.replaceChildren(...introView());
    }
    const chip = $("#session-chip");
    if (s) {
      chip.hidden = false;
      if (s.mode === "host") {
        chip.textContent = `Hosting · ${s.code}`;
        chip.className = "chip ok";
      } else if (s.phase === "connected") {
        chip.textContent = "Connected";
        chip.className = "chip ok";
      } else if (s.phase === "failed") {
        chip.textContent = "Not connected";
        chip.className = "chip bad";
      } else {
        chip.textContent = "Connecting…";
        chip.className = "chip busy";
      }
    } else {
      chip.hidden = true;
    }
    renderSessionPill();
  }

  // ---------- saves ----------

  async function loadSaves() {
    state.saves = await api("/api/saves");
    renderSaves();
  }

  const SLOT_LETTERS = ["A", "B", "C", "D"];

  function slotLabel(i) {
    const name = (state.saves && state.saves.names && state.saves.names[i]) || "";
    return name.trim() ? `${name.trim()} · ${SLOT_LETTERS[i]}` : `MARIO ${SLOT_LETTERS[i]}`;
  }

  function renderSaves() {
    const grid = $("#saves-grid");
    const data = state.saves;
    if (!data) return;
    grid.replaceChildren(...data.slots.map(saveCard));
  }

  function saveCard(slot) {
    const exists = slot.valid && slot.exists;
    const head = el("div", { class: "save-head" },
      el("span", { class: "save-slot" }, slotLabel(slot.index)),
      exists ? el("span", { class: "tag" }, slot.signature) : null
    );

    const body = exists
      ? [
          el("div", { class: "save-stars" }, "★ " + slot.stars),
          el("div", { class: "save-meta" }, `${slot.coins} coins · cap ${slot.capLevel}/${slot.capArea}`),
        ]
      : [el("div", { class: "save-stars" }, "—"), el("div", { class: "save-meta" }, "Empty")];

    const actions = el("div", { class: "save-actions" });
    if (exists) {
      actions.append(
        el("button", { class: "btn", type: "button", onclick: () => openCopyDialog(slot.index) }, "Copy to…"),
        el("button", { class: "btn btn-ghost btn-danger", type: "button", onclick: () => clearSlot(slot) }, "Clear")
      );
    }

    return el("div", { class: "save" + (exists ? "" : " empty") }, head, ...body, actions);
  }

  function openCopyDialog(from) {
    $("#copy-title").textContent = `Copy ${slotLabel(from)} to…`;
    const targets = $("#copy-targets");
    targets.replaceChildren();
    for (let i = 0; i < 4; i++) {
      if (i === from) continue;
      const occupied = state.saves.slots[i] && state.saves.slots[i].exists;
      targets.append(el("button", {
        class: "btn",
        type: "button",
        onclick: () => { $("#copy-dialog").close(); copySlot(from, i); },
      }, `${slotLabel(i)}${occupied ? " (replaces it)" : " (empty)"}`));
    }
    $("#copy-dialog").showModal();
  }

  async function copySlot(from, to) {
    await guard(async () => {
      await postJSON("/api/saves/copy", { from, to });
      await loadSaves();
      toast(`Copied ${slotLabel(from)} to ${slotLabel(to)}.`, "ok");
    });
  }

  async function clearSlot(slot) {
    if (!confirm(`Erase ${slotLabel(slot.index)}?\n\nA backup of the file stays next to it.`)) return;
    await guard(async () => {
      await postJSON("/api/saves/clear", { slot: slot.index });
      await loadSaves();
      toast("Slot erased.", "ok");
    });
  }

  function onSavePicked(ev) {
    const file = ev.target.files && ev.target.files[0];
    ev.target.value = "";
    if (!file) return;
    guard(async () => {
      const form = new FormData();
      form.append("file", file);
      await api("/api/saves/import", { method: "POST", body: form });
      await loadSaves();
      toast("Save imported. The previous file is backed up.", "ok");
    });
  }

  // ---------- profiles ----------

  async function loadProfiles() {
    state.profiles = await api("/api/profiles");
    renderProfiles();
  }

  function renderProfiles() {
    const data = state.profiles;
    const list = $("#profiles-list");
    if (!data) return;
    const rows = (data.names || []).map((name) => {
      const active = name === data.active;
      const count = (data.sets[name] || []).length;
      return el("div", { class: "item" },
        el("div", { class: "item-main" },
          el("div", { class: "item-title" },
            el("strong", {}, name),
            active ? el("span", { class: "tag pack" }, "active") : null
          ),
          el("div", { class: "item-id" }, `${count} mod${count === 1 ? "" : "s"}`)
        ),
        el("div", { class: "item-actions" },
          active ? null : el("button", {
            class: "btn",
            type: "button",
            disabled: mutationBlocked(),
            onclick: () => switchProfile(name),
          }, "Switch"),
          active ? null : el("button", {
            class: "btn btn-ghost btn-danger",
            type: "button",
            onclick: () => deleteProfile(name),
          }, "Delete")
        )
      );
    });
    list.replaceChildren(...rows);
  }

  async function switchProfile(name) {
    await guard(async () => {
      await postJSON("/api/profiles/switch", { name });
      await Promise.all([loadProfiles(), loadMods()]);
      toast(`Switched to ${name}.`, "ok");
    });
  }

  async function deleteProfile(name) {
    if (!confirm(`Delete the profile "${name}"?`)) return;
    await guard(async () => {
      await postJSON("/api/profiles/delete", { name });
      await loadProfiles();
    });
  }

  // ---------- settings ----------

  async function saveGamePath() {
    await guard(async () => {
      await postJSON("/api/settings/gamepath", { path: $("#game-path").value.trim() });
      await refreshStatus();
      toast("Game folder saved.", "ok");
    });
  }

  async function detectGame() {
    await guard(async () => {
      const res = await postJSON("/api/game/detect", {});
      await refreshStatus();
      toast(res.gameFound ? "Found the game." : "Could not find it. Pick the folder by hand.",
        res.gameFound ? "ok" : "bad");
    });
  }

  async function setCloseOnPlay(on) {
    await guard(async () => {
      await postJSON("/api/settings/closeonplay", { on });
      await refreshStatus();
    });
  }

  async function launchGame() {
    // With a room open, the header button must use the room's arguments
    // (--server/--client); a plain launch would bypass the bridge.
    if (state.session && state.session.phase !== "stopped") {
      await guard(launchSession);
      return;
    }
    await guard(async () => {
      await postJSON("/api/game/launch", {});
      toast("Starting the game…", "ok");
      setTimeout(() => refreshStatus().catch(() => {}), 1500);
    });
  }

  // ---------- tabs ----------

  function showTab(name) {
    state.tab = name;
    for (const btn of document.querySelectorAll(".rail-link")) {
      if (btn.dataset.tab === name) btn.setAttribute("aria-current", "page");
      else btn.removeAttribute("aria-current");
    }
    for (const section of document.querySelectorAll(".tab")) {
      section.hidden = section.id !== "tab-" + name;
    }
    if (name === "play") guard(async () => {
      await refreshSession(true);
      const details = $("#session-log-card");
      if (details && details.open) await refreshSessionLogs();
    });
    if (name === "browse") guard(async () => {
      await loadCategories();
      if (!state.browse.loaded) await loadBrowse(true);
    });
    if (name === "saves") guard(loadSaves);
    if (name === "profiles") guard(loadProfiles);
    if (name === "mods") guard(loadMods);
  }

  // ---------- boot ----------

  function bind() {
    for (const btn of document.querySelectorAll(".rail-link")) {
      btn.addEventListener("click", () => showTab(btn.dataset.tab));
    }
    $("#filter").addEventListener("input", (ev) => {
      state.filter = ev.target.value;
      renderMods();
    });
    $("#install-file").addEventListener("change", onInstallPicked);

    $("#browse-q").addEventListener("keydown", (ev) => {
      if (ev.key !== "Enter") return;
      ev.preventDefault();
      state.browse.q = ev.target.value.trim();
      guard(() => loadBrowse(true));
    });
    $("#browse-sort").addEventListener("change", (ev) => {
      state.browse.sort = ev.target.value;
      guard(() => loadBrowse(true));
    });
    $("#browse-cat").addEventListener("change", (ev) => {
      state.browse.category = Number(ev.target.value) || 0;
      guard(() => loadBrowse(true));
    });
    $("#browse-refresh").addEventListener("click", () => guard(async () => {
      await postJSON("/api/catalog/clear-cache", {});
      await loadBrowse(true);
    }));
    $("#browse-more").addEventListener("click", () => guard(async () => {
      if (state.browse.page >= state.browse.pages) return;
      state.browse.page += 1;
      await loadBrowse(false);
    }));
    $("#save-file").addEventListener("change", onSavePicked);
    $("#play").addEventListener("click", launchGame);
    $("#game-save").addEventListener("click", saveGamePath);
    $("#game-detect").addEventListener("click", detectGame);
    $("#close-on-play").addEventListener("change", (ev) => setCloseOnPlay(ev.target.checked));
    $("#session-pill").addEventListener("click", () => showTab("play"));
    $("#session-log-card").addEventListener("toggle", (ev) => {
      if (ev.target.open) guard(refreshSessionLogs);
    });
    $("#profile-create").addEventListener("click", () => guard(async () => {
      const input = $("#profile-name");
      const name = input.value.trim();
      if (!name) return;
      await postJSON("/api/profiles/create", { name });
      input.value = "";
      await loadProfiles();
      toast(`Profile "${name}" created.`, "ok");
    }));
    $("#profile-name").addEventListener("keydown", (ev) => {
      if (ev.key === "Enter") $("#profile-create").click();
    });
  }

  async function boot() {
    bind();
    try {
      await refreshStatus();
      await Promise.all([loadMods(), loadSaves(), loadProfiles()]);
      await refreshSession(true).catch(() => {});
      renderSession();
    } catch (err) {
      toast(err.message || "Could not talk to the loader.", "bad");
    }
    setInterval(() => refreshStatus().catch(() => {}), 5000);
    setInterval(() => {
      if (state.tab === "play" || state.session) refreshSession().catch(() => {});
    }, 1000);
  }

  boot();
})();
