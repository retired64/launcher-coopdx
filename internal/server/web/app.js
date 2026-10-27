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
        el("div", { class: "item-id" }, m.id)
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
