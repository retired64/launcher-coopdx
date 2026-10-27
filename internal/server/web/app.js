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
    $("#close-on-play").checked = !!st.closeOnPlay;

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
    } catch (err) {
      toast(err.message || "Could not talk to the loader.", "bad");
    }
    setInterval(() => refreshStatus().catch(() => {}), 5000);
  }

  boot();
})();
