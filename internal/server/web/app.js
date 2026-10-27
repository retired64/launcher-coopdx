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
    modFilter: "all",
    expandedMods: new Set(),
    tab: "mods",
    rom: null,
    romBusy: false,
    romPoll: null,
    romFonts: null,
    hud: null,
    player: null,
    playerPresetOpen: false,
    playerBusy: false,
    savePick: 0,
    session: null,
    sessionKey: "",
    sessionBusy: false,
    sessionLaunching: false,
    sessionTried: "",
    sessionLaunchedFor: "",
    sessionLaunchedAt: 0,
    lastRunning: false,
    lastSessionGameIn: false,
    copy: null,
    copyKey: "",
    copyBusy: false,
    copyChoice: { mods: true, look: true, saves: true },
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


  // ---------- the look that comes from the player's ROM ----------

  const PAGES = {
    mods: { title: "MIS MODS", view: "mods" },
    browse: { title: "BUSCAR", view: "browse" },
    player: { title: "JUGADOR", view: "player" },
    saves: { title: "PARTIDAS", view: "saves" },
    create: { title: "CREAR", view: "create" },
    settings: { title: "OPCIONES", view: "settings" },
    play: { title: "JUGAR CON AMIGOS", view: "play" },
    copy: { title: "COPIAR ENTRE DISPOSITIVOS", view: "copy" },
    profiles: { title: "PERFILES", view: "mods" },
  };

  function setPage(name) {
    const page = PAGES[name] || PAGES.mods;
    document.documentElement.dataset.view = page.view;
    const title = $("#page-title");
    if (title) title.setAttribute("text", page.title);
  }

  class Sm64Text extends HTMLElement {
    static get observedAttributes() { return ["text", "data-refresh"]; }
    connectedCallback() { this.render(); }
    attributeChangedCallback() { if (this.isConnected) this.render(); }
    render() {
      const text = (this.getAttribute("text") || "").toUpperCase();
      this.setAttribute("role", "img");
      this.setAttribute("aria-label", text);
      this.replaceChildren();
      for (const ch of text) {
        if (ch === " ") {
          const space = document.createElement("span");
          space.className = "sm64-space";
          this.append(space);
          continue;
        }
        const rel = state.hud && state.hud[ch];
        if (rel) {
          const img = new Image();
          img.alt = "";
          img.className = "sm64-glyph";
          img.src = "/rom-assets/" + rel;
          this.append(img);
        } else {
          const span = document.createElement("span");
          span.className = "sm64-fallback";
          span.textContent = ch;
          this.append(span);
        }
      }
    }
  }
  if (!customElements.get("sm64-text")) customElements.define("sm64-text", Sm64Text);

  function refreshSm64Text() {
    for (const node of document.querySelectorAll("sm64-text")) node.render?.();
  }

  async function loadROMFonts() {
    if (state.romFonts) return state.romFonts;
    state.romFonts = (async () => {
      for (const [family, file] of [
        ["SM64 Dialog", "dialog.ttf"],
        ["SM64 Menu", "menu.ttf"],
      ]) {
        try {
          const face = new FontFace(family, `url("/rom-assets/${file}")`);
          await face.load();
          document.fonts.add(face);
        } catch { /* the fallback text still works */ }
      }
    })();
    return state.romFonts;
  }

  async function applyROM(rom) {
    state.rom = rom;
    const ready = !!rom && rom.ready;
    document.documentElement.classList.toggle("rom", ready);
    const stateLine = $("#rom-state");
    if (stateLine) {
      stateLine.textContent = ready
        ? "Archivo de Super Mario 64 (ROM) encontrado. Las imágenes y letras de la app se leen de él."
        : (rom && rom.message) || "Indica dónde está tu ROM de Super Mario 64 (USA) para usar el estilo del juego.";
      stateLine.className = ready ? "caption state ok" : "caption state bad";
    }
    if (!ready) {
      refreshSm64Text();
      return;
    }
    try {
      state.hud = await (await fetch("/rom-assets/hud.json")).json();
    } catch {
      state.hud = {};
    }
    await loadROMFonts();
    refreshSm64Text();
  }

  async function refreshROM(force = false) {
    if (state.romBusy) return;
    state.romBusy = true;
    clearTimeout(state.romPoll);
    try {
      const data = await api("/api/rom/status");
      await applyROM(data.rom);
      if (data.rom && !data.rom.ready && (data.rom.busy || data.rom.reason === "extracting")) {
        state.romPoll = setTimeout(() => {
          state.romBusy = false;
          refreshROM(true);
        }, 900);
        return;
      }
    } catch {
      /* the loader may still be starting */
    } finally {
      state.romBusy = false;
    }
  }

  async function uploadROM(file) {
    if (!file) return;
    try {
      const res = await fetch("/api/rom/upload?name=" + encodeURIComponent(file.name), {
        method: "POST",
        headers: { "X-Token": token, "Content-Type": "application/octet-stream" },
        body: file,
      });
      const data = await res.json().catch(() => null);
      if (!res.ok) throw new Error((data && data.error) || "No se pudo leer la ROM.");
      state.romFonts = null;
      await applyROM(data.rom);
      toast("Estilo del juego cargado desde tu ROM.", "ok");
    } catch (err) {
      toast(err.message || "No se pudo leer la ROM.", "bad");
    }
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
    chip.hidden = true;
    chip.className = "game-card";
    if (st.running) {
      chip.textContent = "Juego abierto";
      chip.classList.add("is-warn");
    } else if (st.gameFound) {
      chip.textContent = "Listo para jugar";
      chip.classList.add("is-ok");
    } else {
      chip.textContent = "Juego no encontrado";
      chip.classList.add("is-bad");
    }

    $("#version").textContent = "v" + st.version;
    $("#play").disabled = !st.gameFound;
    $("#play").textContent = st.running ? "Jugando…" : "Jugar";
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
    const closeWord = $("#close-on-play-word");
    if (closeWord) closeWord.textContent = st.closeOnPlay ? "Cerrar el loader" : "Dejarlo abierto";

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
      gs.className = "settings-value state ok";
    } else {
      gs.textContent = "Aún no se encuentra. Elige la carpeta que tiene sm64coopdx.";
      gs.className = "settings-value state bad";
    }

    $("#path-data").textContent = st.gameData;
    $("#path-config").textContent = st.configPath + (st.configFound ? "" : "  (falta)");
    $("#path-save").textContent = st.savePath + (st.saveFound ? "" : "  (falta)");
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

  const MOD_GROUPS = [
    { key: "romhack", title: "Rom hacks", hint: "Solo uno puede estar activo a la vez." },
    { key: "gamemode", title: "Modos de juego", hint: "Solo uno puede estar activo a la vez." },
    { key: "extra", title: "Extras", hint: "Activa todos los que quieras." },
    { key: "pack", title: "Paquetes de modelos", hint: "Activa todos los que quieras." },
  ];

  function modGroupKey(m) {
    if (m.kind === "pack") return "pack";
    const text = `${m.incompatible || ""} ${m.category || ""}`.toLowerCase();
    if (text.includes("romhack") || text.includes("rom hack")) return "romhack";
    if (text.includes("gamemode") || text.includes("game mode")) return "gamemode";
    return "extra";
  }

  function renderMods() {
    const container = $("#mods-list");
    const q = state.filter.trim().toLowerCase();
    const shown = state.mods.filter((m) => {
      if (state.modFilter === "on" && !m.enabled) return false;
      if (state.modFilter === "off" && m.enabled) return false;
      if (!q) return true;
      return (m.name + " " + m.id + " " + m.description + " " + m.category)
        .toLowerCase()
        .includes(q);
    });

    if (!shown.length) {
      container.replaceChildren(el("p", { class: "boot" },
        q || state.modFilter !== "all" ? "No hay mods que coincidan." :
        "Todavía no hay mods. Busca uno y pulsa Instalar; aparece aquí, activo."));
    } else {
      const groups = [];
      for (const group of MOD_GROUPS) {
        const items = shown.filter((m) => modGroupKey(m) === group.key);
        if (!items.length) continue;
        groups.push(el("section", { class: "group" },
          el("div", { class: "group-head" },
            el("h2", { class: "group-title" }, group.title, el("span", { class: "group-count" }, String(items.length))),
            el("span", { class: "caption" }, group.hint)
          ),
          ...items.map(modRow)
        ));
      }
      container.replaceChildren(...groups);
    }

    const on = state.mods.filter((m) => m.enabled).length;
    const total = state.mods.length;
    const count = $("#mod-count");
    if (count) {
      count.textContent = `${on} activos`;
      count.title = `${on} activos`;
    }
    let hint = total
      ? `${total} instalados · ${on} activos. Los cambios se aplican la próxima vez que abras el juego.`
      : "Instala un .zip del sitio de mods o un archivo .lua que ya tengas.";
    if (state.status && !state.status.configFound) {
      hint += " Abre el juego una vez para que cree su archivo de configuración.";
    }
    if (mutationBlocked()) {
      hint += " Cierra el juego para cambiar los mods.";
    }
    $("#mods-hint").textContent = hint;
  }

  function modRow(m) {
    const key = m.kind + ":" + m.id;
    const expanded = state.expandedMods.has(key);
    const tags = [
      m.kind === "pack" ? el("span", { class: "tag pack" }, "DynOS") : null,
      m.origin === "builtin" ? el("span", { class: "tag builtin" }, "incluido") : null,
      m.category ? el("span", { class: "tag" }, m.category) : null,
    ].filter(Boolean);

    const word = el("span", { class: "switch-word" }, m.enabled ? "Activo" : "Inactivo");
    const toggle = el("label", { class: "switch", title: m.enabled ? "Desactivar" : "Activar" },
      el("input", {
        type: "checkbox",
        checked: m.enabled,
        disabled: m.origin === "builtin" || mutationBlocked(),
        onchange: (ev) => {
          word.textContent = ev.target.checked ? "Activo" : "Inactivo";
          toggleMod(m, ev.target.checked);
        },
      }),
      el("span", { class: "knob", "aria-hidden": "true" }),
      word
    );

    const actions = [];
    if (m.description || m.site) {
      actions.push(el("button", {
        class: "btn btn-sm",
        type: "button",
        onclick: () => {
          if (state.expandedMods.has(key)) state.expandedMods.delete(key);
          else state.expandedMods.add(key);
          renderMods();
        },
      }, expanded ? "Menos" : "Más"));
    }
    actions.push(toggle);
    if (m.origin === "user" && expanded) {
      actions.push(el("button", {
        class: "btn btn-sm",
        type: "button",
        disabled: mutationBlocked(),
        onclick: () => removeMod(m),
      }, "Quitar"));
    }

    return el("div", { class: "item" + (m.enabled ? " is-on" : "") + (expanded ? " expanded" : "") },
      el("div", { class: "item-main" },
        el("div", { class: "item-title" }, el("strong", {}, m.name), ...tags),
        m.description ? el("p", { class: "item-desc" }, m.description) : null,
        el("div", { class: "item-id" },
          m.site ? `${m.id} · sitio ${m.site.version}` : m.id,
          m.incompatible ? ` · incompatible: ${m.incompatible}` : ""
        )
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
    list.classList.toggle("has-error", !!b.error);
    if (!b.loaded && b.loading) {
      list.replaceChildren(el("p", { class: "boot" }, "Buscando en el sitio de mods…"));
      $("#browse-more").hidden = true;
      return;
    }
    if (b.error) {
      const why = String(b.error)
        .replace(/the mod site answered/i, "el sitio de mods respondió")
        .replace(/the mod site/i, "el sitio de mods");
      list.replaceChildren(el("div", { class: "notice error" },
        el("span", {}, `No se pudo conectar con el sitio de mods. ${why}`),
        el("button", { class: "btn btn-sm", type: "button", onclick: () => guard(() => loadBrowse(true)) }, "Intentar de nuevo")
      ));
      $("#browse-more").hidden = true;
      return;
    }

    list.replaceChildren(...b.items.map(browseRow));
    if (!b.items.length && b.loaded) {
      list.append(el("p", { class: "boot" }, "No hay mods que coincidan con esa búsqueda."));
    }
    $("#browse-more").hidden = b.page >= b.pages || b.loading;
    $("#browse-hint").textContent = b.loaded
      ? `Página ${b.page} de ${b.pages}${b.fromCache ? " · en caché" : ""}. Haz clic en un título para leer el mod.`
      : "Los mods vienen de mods.sm64coopdx.com. El loader pregunta antes de instalar.";
  }

  function browseRow(item) {
    const status = browseStatus(item);
    const install = el("button", {
      class: "btn" + (status === "update" ? " btn-accent" : ""),
      type: "button",
      disabled: status === "installed",
    }, status === "installed" ? "Instalado" : status === "update" ? "Actualizar" : "Instalar");
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
          status === "update" ? el("span", { class: "tag pack" }, "actualizar") : null,
          status === "found" ? el("span", { class: "tag", title: "Ya hay un mod con este nombre en tu carpeta. Al instalarlo desde aquí, el loader recordará su procedencia." }, "encontrado") : null
        ),
        el("div", { class: "browse-sub" }, [item.author, item.category].filter(Boolean).join(" · ")),
        item.tagline ? el("p", { class: "browse-tagline" }, item.tagline) : null,
        el("div", { class: "browse-stats" },
          item.rating ? el("span", {}, `★ ${Number(item.rating).toFixed(1)}${item.ratingCount ? ` (${item.ratingCount})` : ""}`) : null,
          item.downloads ? el("span", {}, `${Number(item.downloads).toLocaleString()} descargas`) : null
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
      el("div", { class: "detail-head" }, title, el("button", { class: "btn btn-ghost", type: "button", onclick: close }, "Cerrar")),
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
    }, status === "installed" ? "Instalado" : status === "update" ? "Actualizar" : "Instalar");
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
        el("button", { class: "btn", type: "button", onclick: close }, "Cancelar")
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
        el("button", { class: "btn", type: "button", onclick: close }, "Cancelar"),
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
        el("button", { class: "btn btn-ghost", type: "button", onclick: close }, "Cerrar")
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
      toast("Código copiado.", "ok");
    } catch {
      toast("No se pudo copiar el código. Selecciónalo y cópialo a mano.", "bad");
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
            toast(err.message || "No se pudo guardar esa elección.", "bad");
            resolve(false);
            return;
          }
        }
        resolve(accepted);
      };
      dialog.append(
        el("h3", {}, "Jugar con amigos usando el asistente de conexión"),
        el("p", {},
          "El asistente de conexión une tu juego con el de tus amigos a través de esta app, así nadie necesita una VPN, configurar el router ni CoopNet."
        ),
        el("ul", { class: "consent-list" },
          el("li", {}, "Tú recibes un código y tu amigo lo escribe."),
          el("li", {}, "Las dos apps se encuentran en trackers públicos de WebTorrent y en el relé de esta app en Cloudflare. Lo que se envían va cerrado con tu código."),
          el("li", {}, "Tu dirección de internet se comparte con esos puntos de encuentro, el relé y los servicios de búsqueda de dirección de Google y Cloudflare. El tráfico del juego solo va directo a tu amigo; el relé se usa solo cuando una ruta directa es imposible."),
          el("li", {}, "Al unirte, el juego del anfitrión manda sus mods activos al tuyo y tu juego ejecuta su Lua. Únete solo con gente de confianza.")
        ),
        el("p", { class: "hint" },
          "Si dices que no, igual puedes jugar en línea como siempre: CoopNet o Conexión directa en el menú del juego."
        ),
        el("div", { class: "dialog-actions" },
          el("button", { class: "btn", type: "button", onclick: () => finish(false) }, "Ahora no"),
          el("button", { class: "btn btn-accent", type: "button", onclick: () => finish(true) }, "Usar el asistente de conexión")
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
    let text = "Sesión";
    if (s.mode === "host") text = `Anfitrión · ${s.code}`;
    else if (s.phase === "connected") text = "Jugando con un amigo";
    else if (s.phase === "failed") text = "Sin conexión";
    else text = "Conectando con un amigo…";
    if (s.phase === "stopped") text = "Sesión detenida";
    pill.textContent = text;
  }

  async function refreshSessionLogs() {
    const data = await api("/api/session/logs");
    const lines = data.logs || [];
    $("#session-log").textContent = lines.length
      ? lines.slice(-400).join("\n")
      : "Todavía no hay líneas en el registro.";
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
      toast("Eso no parece un código. Tiene 8 letras y números, como K7QD-9XPA.", "bad");
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
      toast(err.message || "No se pudo detener la sesión.", "bad");
    } finally {
      state.session = null;
      state.sessionKey = "";
      state.sessionTried = "";
      state.sessionLaunchedFor = "";
      state.lastSessionGameIn = false;
      state.sessionBusy = false;
      renderSession();
    }
    if (!quiet) toast("Sesión detenida.", "ok");
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
      toast("Abriendo el juego…", "ok");
    } catch (err) {
      toast(err.message || "No se pudo abrir el juego.", "bad");
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
    if (s.phase === "failed") return s.message || "No se pudo empezar como anfitrión.";
    if (s.trackers === 0) return "Conectándose…";
    if (!s.friends.length) return "En línea. Esperando que se unan tus amigos…";
    return s.friends.length === 1
      ? "1 amigo está jugando."
      : `${s.friends.length} amigos están jugando.`;
  }

  function friendRows(friends) {
    if (!friends || !friends.length) {
      return el("p", { class: "caption" }, "Nadie todavía. Se unen con tu código.");
    }
    return el("div", { class: "session-friends" }, ...friends.map((f) => {
      const route = f.reconnecting
        ? "reconectando"
        : f.route === "relay"
          ? "relé"
          : f.route === "direct"
            ? "directo"
            : "ruta desconocida";
      return el("div", { class: "session-friend" },
        el("span", { class: "session-friend-id" }, String(f.id).slice(0, 6)),
        el("span", { class: "tag " + (f.reconnecting ? "pack" : "") }, route),
        el("span", { class: "caption" },
          f.game === true ? "juego dentro" : f.game === false ? "juego sin abrir" : "esperando su juego"
        )
      );
    }));
  }

  function autoChoice() {
    return el("label", { class: "field-label" }, "El juego se abre",
      el("select", {
        class: "input",
        onchange: (ev) => {
          state.sessionAuto = ev.target.value === "auto";
          try { localStorage.setItem(AUTO_OPEN, state.sessionAuto ? "on" : "off"); } catch { /* ignore */ }
        },
      },
        el("option", { value: "auto", selected: state.sessionAuto ? "selected" : null }, "Solo"),
        el("option", { value: "manual", selected: state.sessionAuto ? null : "selected" }, "Cuando yo elija")
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
      el("h3", {}, "Ser anfitrión"),
      el("p", {}, "Tú recibes un código. Tus amigos lo escriben y sus juegos se unen al tuyo."),
      el("div", { class: "session-options" },
        el("label", { class: "field-label" }, "Archivo de guardado", hostSlot),
        el("label", { class: "field-label" }, "Jugadores", hostPlayers),
        autoChoice()
      ),
      el("div", { class: "session-actions" },
        el("button", {
          class: "btn btn-accent",
          type: "button",
          disabled: state.sessionBusy,
          onclick: () => guard(startHost),
        }, state.sessionBusy ? "Empezando…" : "Ser anfitrión")
      ),
      el("p", { class: "hint" }, "El juego de tu amigo descarga tus mods activos al unirse. El juego se abre en el puerto 7777.")
    );

    const input = el("input", {
      class: "input join-input",
      type: "text",
      maxlength: "12",
      spellcheck: "false",
      autocomplete: "off",
      placeholder: "ABCD-1234",
      "aria-label": "Código de tu amigo",
      value: state.joinDraft || "",
      oninput: (ev) => { state.joinDraft = ev.target.value; },
      onkeydown: (ev) => {
        if (ev.key === "Enter") { ev.preventDefault(); guard(() => startJoin(ev.target.value)); }
      },
    });

    const recents = recentCodes();
    const recentRow = recents.length
      ? el("div", { class: "recent-codes" },
          el("span", { class: "caption" }, "Recientes:"),
          ...recents.map((code) => el("button", {
            class: "btn btn-sm",
            type: "button",
            onclick: () => guard(() => startJoin(code)),
          }, code))
        )
      : null;

    const joinCard = el("div", { class: "session-card" },
      el("h3", {}, "Unirse a una partida"),
      el("p", {}, "Escribe el código que recibió tu amigo al apretar Ser anfitrión."),
      input,
      recentRow,
      el("div", { class: "session-actions" },
        el("button", {
          class: "btn btn-accent",
          type: "button",
          disabled: state.sessionBusy,
          onclick: () => guard(() => startJoin(input.value)),
        }, state.sessionBusy ? "Empezando…" : "Unirse")
      ),
      el("p", { class: "hint" }, "El código es la llave de la sala. Trátalo como una contraseña.")
    );

    return [el("div", { class: "session-grid" }, hostCard, joinCard)];
  }

  function hostView(s) {
    const copy = el("button", { class: "btn", type: "button", onclick: () => copyCode(s.code) }, "Copiar código");
    const launched = state.sessionLaunchedFor === s.code;
    const open = el("button", {
      class: "btn btn-accent",
      type: "button",
      disabled: s.gameIn || launched || state.sessionLaunching || !!(state.status && state.status.running),
      onclick: () => guard(launchSession),
    }, s.gameIn ? "El juego está abierto" : launched ? "Esperando tu juego…" : (state.status && state.status.running) ? "El juego está abierto" : state.sessionLaunching ? "Abriendo…" : "Abrir el juego");

    const runningNote = !s.gameIn && state.status && state.status.running
      ? el("p", { class: "hint" }, "El juego ya está abierto sin la sala. Ciérralo y ábrelo desde aquí.")
      : null;

    const options = s.gameIn ? null : el("div", { class: "session-options" },
      el("label", { class: "field-label" }, "Archivo de guardado",
        el("select", {
          class: "input",
          onchange: (ev) => { state.host.slot = Number(ev.target.value); },
        }, ...[1, 2, 3, 4].map((n) => el("option", { value: String(n), selected: state.host.slot === n ? "selected" : null }, String(n))))
      ),
      el("label", { class: "field-label" }, "Jugadores",
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
          el("p", { class: "session-kicker" }, "Anfitrión"),
          el("code", { class: "session-code", tabindex: "0" }, s.code)
        ),
        el("div", { class: "session-head-actions" }, copy)
      ),
      sessionStep(1, "Manda este código a tus amigos",
        el("p", { class: "status-line" }, hostStatus(s))
      ),
      sessionStep(2, "Abrir el juego", options,
        el("div", { class: "session-actions" }, open),
        runningNote
      ),
      sessionStep(3, "Tus amigos", friendRows(s.friends)),
      el("div", { class: "session-actions" },
        el("button", {
          class: "btn btn-danger",
          type: "button",
          disabled: state.sessionBusy,
          onclick: () => { if (confirm("¿Dejar de ser anfitrión? Tus amigos salen de la partida.")) guard(() => stopSession()); },
        }, "Dejar de ser anfitrión")
      ),
      el("p", { class: "hint" }, s.trackers > 0
        ? `${s.trackers} punto${s.trackers === 1 ? "" : "s"} de encuentro en línea.`
        : "Conectándose a los puntos de encuentro…")
    ];
  }

  function joinStatus(s) {
    if (s.phase === "failed") return s.message || "No se pudo conectar.";
    if (s.phase === "connected") {
      const f = s.friends && s.friends[0];
      if (f && f.reconnecting) return "Se cortó la conexión. Reconectando… Tu lugar en el juego se guarda por un minuto.";
      if (f && f.route === "relay") return "Amigo encontrado, por el relé.";
      return "Amigo encontrado.";
    }
    if (s.trackers === 0) return "Conectándose…";
    return "Buscando a tu amigo…";
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
    }, s.gameIn ? "El juego está abierto" : launched ? "Esperando tu juego…" : (state.status && state.status.running) ? "El juego está abierto" : state.sessionLaunching ? "Abriendo…" : "Abrir el juego");

    const gameLine = s.gameIn
      ? "Tu juego ya entró."
      : f && f.game === false
        ? "Tu amigo todavía no abre su juego."
        : ready
          ? "Tu amigo ya está. Abre el juego cuando quieras."
          : "Podrás abrirlo cuando se encuentre a tu amigo.";

    const actions = s.phase === "failed"
      ? [el("button", {
          class: "btn btn-accent",
          type: "button",
          onclick: () => guard(async () => {
            state.joinDraft = s.code;
            await stopSession(true);
          }),
        }, "Intentar de nuevo")]
      : [el("button", {
          class: "btn",
          type: "button",
          disabled: state.sessionBusy,
          onclick: () => guard(() => stopSession()),
        }, "Salir")];

    return [
      el("div", { class: "session-head" },
        el("div", {},
          el("p", { class: "session-kicker" }, "Uniéndose"),
          el("code", { class: "session-code", tabindex: "0" }, s.code)
        )
      ),
      sessionStep(1, "El código de tu amigo",
        el("p", { class: "status-line" }, joinStatus(s))
      ),
      sessionStep(2, "Abrir el juego",
        el("p", { class: "caption" }, gameLine),
        el("p", { class: "hint" },
          "Antes de entrar, tu juego descarga los mods activos de tu amigo que no tengas. La primera unión puede tardar."
        ),
        autoChoice(),
        el("div", { class: "session-actions" }, open),
        !s.gameIn && state.status && state.status.running
          ? el("p", { class: "hint" }, "El juego ya está abierto sin la sala. Ciérralo y ábrelo desde aquí.")
          : null
      ),
      el("div", { class: "session-actions" }, ...actions),
      el("p", { class: "hint" }, s.trackers > 0
        ? `${s.trackers} punto${s.trackers === 1 ? "" : "s"} de encuentro en línea.`
        : "Conectándose a los puntos de encuentro…")
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
        chip.textContent = "Conectado";
        chip.className = "chip ok";
      } else if (s.phase === "failed") {
        chip.textContent = "Sin conexión";
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


  // ---------- copy between devices ----------

  const COPY_CONSENT = "copyConsent";
  const COPY_MODELS = ["Mario", "Luigi", "Toad", "Waluigi", "Wario"];

  function copyConsentGiven() {
    try { return localStorage.getItem(COPY_CONSENT) === "1"; } catch { return false; }
  }

  function ensureCopyConsent() {
    if (copyConsentGiven()) return Promise.resolve(true);
    return new Promise((resolve) => {
      let answered = false;
      const dialog = el("dialog", { class: "dialog session-consent" });
      const finish = (ok) => {
        if (answered) return;
        answered = true;
        dialog.close();
        dialog.remove();
        if (ok) {
          try { localStorage.setItem(COPY_CONSENT, "1"); } catch { /* ignore */ }
        }
        resolve(ok);
      };
      dialog.append(
        el("h3", {}, "Copiar entre dispositivos"),
        el("p", {},
          "Un dispositivo muestra un código y el otro lo escribe. Los dos se encuentran en los mismos puntos de encuentro que Jugar con amigos, y lo que se envían va cerrado con el código."
        ),
        el("ul", { class: "consent-list" },
          el("li", {}, "Después se conectan directo. Cuando eso no se puede, la copia pasa por el relé en Cloudflare."),
          el("li", {}, "Tu dirección de internet se comparte con esos puntos de encuentro, el relé y los servicios de búsqueda de dirección de Google y Cloudflare. Tus archivos solo viajan entre tus dos dispositivos, cifrados."),
          el("li", {}, "Los mods incluyen Lua, y el otro dispositivo lo ejecutará. Copia solo entre dispositivos de confianza.")
        ),
        el("div", { class: "dialog-actions" },
          el("button", { class: "btn", type: "button", onclick: () => finish(false) }, "Ahora no"),
          el("button", { class: "btn btn-accent", type: "button", onclick: () => finish(true) }, "Continuar")
        )
      );
      dialog.addEventListener("cancel", (ev) => { ev.preventDefault(); finish(false); });
      document.body.append(dialog);
      dialog.showModal();
    });
  }

  function copyBytes(n) {
    if (!Number.isFinite(n) || n <= 0) return "0 KB";
    if (n >= 1e9) return `${(n / 1e9).toFixed(1)} GB`;
    if (n >= 1e6) return `${Math.round(n / 1e6)} MB`;
    return `${Math.max(1, Math.round(n / 1e3))} KB`;
  }

  function copyList(list, max = 3) {
    if (!list || !list.length) return "";
    return list.length <= max
      ? list.join(", ")
      : `${list.slice(0, max).join(", ")} and ${list.length - max} more`;
  }

  async function refreshCopy(force) {
    let data;
    try {
      data = await api("/api/copy/state");
    } catch {
      return;
    }
    const next = data.copy || null;
    const key = next ? JSON.stringify(next) : "";
    const changed = key !== state.copyKey;
    if (next && next.phase === "review" && (!state.copy || state.copy.phase !== "review")) {
      state.copyChoice = { mods: true, look: true, saves: true };
    }
    state.copy = next;
    state.copyKey = key;
    if (changed || force) renderCopy();
    if (state.tab === "copy") {
      const details = $("#copy-log-card");
      if (details && details.open) refreshCopyLogs().catch(() => {});
    }
  }

  async function refreshCopyLogs() {
    const data = await api("/api/copy/logs");
    const lines = data.logs || [];
    $("#copy-log").textContent = lines.length
      ? lines.slice(-400).join("\n")
      : "Todavía no hay líneas en el registro.";
  }

  async function startCopy(role, code) {
    if (state.copyBusy) return;
    if (!(await ensureCopyConsent())) return;
    state.copyBusy = true;
    renderCopy();
    try {
      const data = await postJSON("/api/copy/start", { role, code: code || "" });
      state.copy = data.copy;
      state.copyKey = JSON.stringify(data.copy);
      if (state.copy && state.copy.phase === "review") {
        state.copyChoice = { mods: true, look: true, saves: true };
      }
      renderCopy();
    } catch (err) {
      toast(err.message || "Could not start the copy.", "bad");
    } finally {
      state.copyBusy = false;
      renderCopy();
    }
  }

  async function stopCopy(quiet) {
    if (state.copyBusy) return;
    state.copyBusy = true;
    try {
      await postJSON("/api/copy/stop", {});
    } catch (err) {
      if (!quiet) toast(err.message || "Could not stop the copy.", "bad");
    } finally {
      state.copy = null;
      state.copyKey = "";
      state.copyBusy = false;
      renderCopy();
    }
    if (!quiet) toast("Copy stopped.", "ok");
  }

  async function acceptCopy() {
    if (!state.copy || state.copyBusy) return;
    const choice = { ...state.copyChoice };
    if (!choice.mods && !choice.look && !choice.saves) return;
    state.copyBusy = true;
    renderCopy();
    try {
      await postJSON("/api/copy/accept", choice);
    } catch (err) {
      toast(err.message || "Could not start copying.", "bad");
    } finally {
      state.copyBusy = false;
      renderCopy();
    }
  }

  function copyBar(progress) {
    if (!progress) return el("div", { class: "copy-bar", hidden: true });
    const total = Math.max(1, Number(progress.total) || 0);
    const done = Math.max(0, Math.min(total, Number(progress.done) || 0));
    const pct = Math.round((done / total) * 100);
    return el("div", { class: "copy-bar", role: "progressbar", "aria-valuemin": "0", "aria-valuemax": "100", "aria-valuenow": String(pct) },
      el("span", { class: "copy-bar-fill", style: `width:${pct}%` })
    );
  }

  function copyStatus(s) {
    if (s.phase === "failed") return s.message || "No se pudo conectar.";
    if (s.peer) {
      if (s.route === "relay") return `Conectado con ${s.peer.name}, por el relé.`;
      return `Conectado con ${s.peer.name}.`;
    }
    if (s.trackers > 0) return "En línea. Esperando tu otro dispositivo…";
    return "Conectándose…";
  }

  function copyProgressText(s) {
    if (s.phase === "done") {
      const name = s.peer?.name ?? "your other device";
      return s.role === "send"
        ? `Done. ${name} has the mods, look and saves of this computer.`
        : `Done. Your mods, look and saves match ${name}.`;
    }
    if (s.phase === "copying" && s.progress) {
      return `${s.role === "send" ? "Enviando" : "Copiando"} ${copyBytes(s.progress.done)} de ${copyBytes(s.progress.total)}`;
    }
    if (s.peer) return "Está revisando qué copiar.";
    return "Nada todavía.";
  }

  function copyStep(n, title, ...kids) {
    return el("section", { class: "step" },
      el("div", { class: "step-head" },
        el("span", { class: "step-num", "aria-hidden": "true" }, String(n)),
        el("h3", { class: "step-title" }, title)
      ),
      ...kids
    );
  }

  function copyActions(s) {
    if (s.phase === "done") {
      return [el("button", { class: "btn btn-accent", type: "button", onclick: () => guard(() => stopCopy(true)) }, "Listo")];
    }
    if (s.phase === "failed") {
      return [
        el("button", { class: "btn", type: "button", onclick: () => guard(() => stopCopy(true)) }, "Cerrar"),
        el("button", {
          class: "btn btn-accent",
          type: "button",
          onclick: () => guard(async () => {
            const role = s.role;
            const code = s.role === "get" ? s.code : "";
            await stopCopy(true);
            await startCopy(role, code);
          }),
        }, "Intentar de nuevo"),
      ];
    }
    return [el("button", {
      class: "btn",
      type: "button",
      disabled: state.copyBusy,
      onclick: () => guard(() => stopCopy()),
    }, "Detener")];
  }

  function copyIntro() {
    const input = el("input", {
      class: "input join-input",
      type: "text",
      maxlength: "12",
      spellcheck: "false",
      autocomplete: "off",
      placeholder: "ABCD-1234",
      "aria-label": "Code from your other device",
      value: state.joinDraft || "",
      oninput: (ev) => { state.joinDraft = ev.target.value; },
      onkeydown: (ev) => {
        if (ev.key === "Enter") {
          ev.preventDefault();
          const code = normalizeCode(ev.target.value);
          if (!code) { toast("Eso no parece un código. Tiene 8 letras y números, como K7QD-9XPA.", "bad"); return; }
          guard(() => startCopy("get", code));
        }
      },
    });

    const sendCard = el("div", { class: "copy-card" },
      el("h3", {}, "Enviar desde este computador"),
      el("p", {}, "Tu otro dispositivo recibe los mods, el aspecto y las partidas guardadas de este computador."),
      el("div", { class: "session-actions" },
        el("button", {
          class: "btn btn-accent",
          type: "button",
          disabled: state.copyBusy,
          onclick: () => guard(() => startCopy("send", "")),
        }, state.copyBusy ? "Empezando…" : "Enviar")
      )
    );

    const getCard = el("div", { class: "copy-card" },
      el("h3", {}, "Recibir de otro dispositivo"),
      el("p", {}, "Este computador recibe los mods, el aspecto y las partidas guardadas de tu otro dispositivo."),
      input,
      el("div", { class: "session-actions" },
        el("button", {
          class: "btn btn-accent",
          type: "button",
          disabled: state.copyBusy,
          onclick: () => {
            const code = normalizeCode(input.value);
            if (!code) { toast("Eso no parece un código. Tiene 8 letras y números, como K7QD-9XPA.", "bad"); return; }
            guard(() => startCopy("get", code));
          },
        }, state.copyBusy ? "Empezando…" : "Conectar")
      )
    );

    return [
      el("div", { class: "copy-grid" }, sendCard, getCard),
      el("p", { class: "hint" },
        "Abre Coop Loader en los dos dispositivos. Funciona entre computadores y teléfonos, en la misma casa o lejos. El código es la llave: trátalo como una contraseña."
      ),
    ];
  }

  function copySendView(s) {
    const code = el("code", { class: "session-code", tabindex: "0" }, s.code);
    const copy = el("button", {
      class: "btn",
      type: "button",
      onclick: async () => {
        try {
          await navigator.clipboard.writeText(s.code);
          toast("Código copiado.", "ok");
        } catch {
          toast("No se pudo copiar el código. Selecciónalo y cópialo a mano.", "bad");
        }
      },
    }, "Copiar código");

    return [
      el("div", { class: "session-head" },
        el("div", {},
          el("p", { class: "session-kicker" }, "Enviando"),
          code
        ),
        el("div", { class: "session-head-actions" }, copy)
      ),
      copyStep(1, "Escribe este código en tu otro dispositivo",
        el("p", { class: "status-line" }, copyStatus(s)),
        el("p", { class: "caption" }, "Allá: Opciones › Copiar entre dispositivos › Recibir de otro dispositivo.")
      ),
      copyStep(2, "Tu otro dispositivo copia",
        el("p", { class: "caption" }, copyProgressText(s)),
        copyBar(s.progress)
      ),
      el("div", { class: "session-actions" }, ...copyActions(s)),
      s.message ? el("p", { class: "hint" }, s.message) : null,
      s.phase !== "done"
        ? el("p", { class: "hint" },
            s.trackers > 0
              ? `${s.trackers} punto${s.trackers === 1 ? "" : "s"} de encuentro en línea.`
              : "Conectándose a los puntos de encuentro…"
          )
        : null,
    ];
  }

  function copyGetView(s) {
    return [
      el("div", { class: "session-head" },
        el("div", {},
          el("p", { class: "session-kicker" }, "Recibiendo"),
          el("code", { class: "session-code", tabindex: "0" }, s.code)
        )
      ),
      copyStep(1, "El código de tu otro dispositivo",
        el("p", { class: "status-line" }, copyStatus(s)),
        s.phase === "failed" ? el("p", { class: "caption" }, s.message || "") : null
      ),
      copyStep(2, "Este dispositivo copia",
        el("p", { class: "caption" }, copyProgressText(s)),
        copyBar(s.progress)
      ),
      el("div", { class: "session-actions" }, ...copyActions(s)),
      s.message ? el("p", { class: "hint" }, s.message) : null,
      s.phase !== "done"
        ? el("p", { class: "hint" },
            s.trackers > 0
              ? `${s.trackers} punto${s.trackers === 1 ? "" : "s"} de encuentro en línea.`
              : "Conectándose a los puntos de encuentro…"
          )
        : null,
    ];
  }

  function copyCheckbox(key, label, description) {
    return el("label", { class: "copy-check" },
      el("input", {
        type: "checkbox",
        checked: state.copyChoice[key] ? "checked" : null,
        onchange: (ev) => {
          state.copyChoice[key] = ev.target.checked;
          renderCopy();
        },
      }),
      el("span", {},
        el("strong", {}, label),
        description ? el("span", { class: "caption" }, description) : null
      )
    );
  }

  function copyReviewView(s) {
    const r = s.review;
    const mods = {
      added: r.mods.added || [],
      updated: r.mods.updated || [],
      on: r.mods.on || 0,
      bytes: r.mods.bytes || 0,
      turnedOff: r.mods.turnedOff || [],
      missing: r.mods.missing || [],
      skipped: r.mods.skipped || [],
      compiled: r.mods.compiled || [],
    };
    const look = {
      name: r.look.name || "",
      model: r.look.model || 0,
      colors: r.look.colors || {},
      presets: r.look.presets || [],
    };
    const saves = { slots: r.saves.slots || [], modSaves: r.saves.modSaves || 0 };
    const changedMods = [...mods.added, ...mods.updated];
    const modLines = [];
    if (changedMods.length) {
      modLines.push(`${changedMods.length} por copiar: ${copyList(changedMods, 2)}.`);
      modLines.push(`${copyBytes(mods.bytes)}. ${mods.on === 1 ? "1 quedará activo." : `${mods.on} quedarán activos.`}`);
    } else {
      modLines.push("Ya los tienes todos.");
      modLines.push(mods.on === 1 ? "1 quedará activo." : `${mods.on} quedarán activos.`);
    }
    if (mods.turnedOff.length) modLines.push(`Quedan inactivos, solo en este computador (no se borran): ${copyList(mods.turnedOff, 2)}.`);
    if (mods.missing.length) modLines.push(`No están en este juego, quedan fuera: ${copyList(mods.missing, 2)}.`);
    if (mods.skipped.length) {
      modLines.push(`Muy grandes para el relé, quedan fuera: ${copyList(mods.skipped, 2)}.`);
      modLines.push("Conecta los dos dispositivos al mismo Wi-Fi para copiarlos.");
    }
    if (mods.compiled.length) modLines.push(`Tiene código compilado (no se puede leer antes de que corra): ${copyList(mods.compiled, 2)}.`);

    const color = (key) => el("span", { class: "copy-color", style: `background:${look.colors[key] || "#808080"}` });
    const lookLine = el("span", { class: "copy-look" },
      color("cap"), color("shirt"), color("pants"),
      el("span", {}, `${look.name || "Jugador"} · ${COPY_MODELS[look.model] || "Mario"}`)
    );

    const saveLines = [];
    for (const slot of saves.slots) {
      const here = slot.here;
      const stars = slot.stars;
      let text = null;
      if (stars === null && here !== null) text = `Archivo de guardado ${slot.slot + 1}: vacío, reemplaza ${here} estrella${here === 1 ? "" : "s"}.`;
      else if (stars !== null && here === null) text = `Archivo de guardado ${slot.slot + 1}: ${stars} estrella${stars === 1 ? "" : "s"}, nuevo aquí.`;
      else if (stars !== null && here !== null) text = `Archivo de guardado ${slot.slot + 1}: ${stars} estrella${stars === 1 ? "" : "s"}, reemplaza ${here} estrella${here === 1 ? "" : "s"}.`;
      if (text) {
        const lose = here !== null && (stars ?? 0) < here;
        saveLines.push(lose ? el("strong", { class: "copy-warn" }, text) : text);
      }
    }
    if (saves.modSaves) saveLines.push(saves.modSaves === 1 ? "Y 1 partida guardada de un mod." : `Y ${saves.modSaves} partidas guardadas de mods.`);
    if (saves.slots.some((slot) => slot.here !== null && (slot.stars ?? 0) < slot.here)) {
      saveLines.push("Apaga Partidas para conservar las estrellas de este computador.");
    }
    if (!saveLines.length) saveLines.push("Todavía no hay partidas guardadas allá.");

    const any = state.copyChoice.mods || state.copyChoice.look || state.copyChoice.saves;
    return [
      el("div", { class: "session-head" },
        el("div", {},
          el("p", { class: "session-kicker" }, "Revisar"),
          el("h3", {}, `Copiar desde ${r.from.name}`)
        )
      ),
      el("div", { class: "copy-review" },
        el("section", { class: "copy-part" },
          copyCheckbox("mods", "Mods", modLines.join(" "))
        ),
        el("section", { class: "copy-part" },
          copyCheckbox("look", "Aspecto", look.presets.length ? `Presets: ${copyList(look.presets, 2)}.` : "Nombre, personaje y colores."),
          lookLine
        ),
        el("section", { class: "copy-part" },
          copyCheckbox("saves", "Partidas", ""),
          el("div", { class: "copy-save-lines" }, ...saveLines.map((line) => el("p", { class: "caption" }, line)))
        )
      ),
      el("p", { class: "caption" },
        "Lo que la copia reemplaza se guarda en removed-by-coop-loader, no se borra."
      ),
      el("div", { class: "session-actions" },
        el("button", { class: "btn", type: "button", onclick: () => guard(() => stopCopy()) }, "Cancelar"),
        el("button", {
          class: "btn btn-accent",
          type: "button",
          disabled: !any || state.copyBusy,
          onclick: () => guard(acceptCopy),
        }, state.copyBusy ? "Empezando…" : "Copiar a este computador")
      ),
    ];
  }

  function renderCopy() {
    const panel = $("#copy-panel");
    const s = state.copy;
    if (!panel) return;
    let kids;
    if (!s) kids = copyIntro();
    else if (s.role === "send") kids = copySendView(s);
    else if (s.phase === "review" && s.review) kids = copyReviewView(s);
    else kids = copyGetView(s);
    panel.replaceChildren(...kids.filter((kid) => kid !== null && kid !== undefined && kid !== false));

    const chip = $("#copy-chip");
    if (s) {
      chip.hidden = false;
      if (s.phase === "done") { chip.textContent = "Listo"; chip.className = "chip ok"; }
      else if (s.phase === "failed") { chip.textContent = "Sin conexión"; chip.className = "chip bad"; }
      else if (s.peer) { chip.textContent = "Conectado"; chip.className = "chip ok"; }
      else { chip.textContent = "Esperando"; chip.className = "chip busy"; }
    } else {
      chip.hidden = true;
    }
  }


  // ---------- player look ----------

  const PALETTE_KEYS = [
    ["shirt", "Camiseta"], ["pants", "Overol"], ["cap", "Gorra"], ["emblem", "Emblema"],
    ["gloves", "Guantes"], ["shoes", "Zapatos"], ["skin", "Piel"], ["hair", "Pelo"],
  ];

  async function loadPlayer() {
    try {
      state.player = await api("/api/player");
      state.playerPresetOpen = false;
      renderPlayer();
    } catch (err) {
      if (err.code === "no-config") {
        state.player = null;
        renderPlayer();
      } else {
        toast(err.message || "No se pudo leer tu jugador.", "bad");
      }
    }
  }

  function presetChip(colors) {
    return el("span", { class: "preset-chip", "aria-hidden": "true" },
      el("span", { style: `background:${colors.cap}` }),
      el("span", { style: `background:${colors.shirt}` }),
      el("span", { style: `background:${colors.pants}` })
    );
  }

  function playerGroup(title, hint, ...kids) {
    return el("section", { class: "player-group" },
      el("div", { class: "group-head" },
        el("h2", { class: "group-title" }, title),
        hint ? el("span", { class: "caption" }, hint) : null
      ),
      ...kids
    );
  }

  function playerRow(main, control) {
    return el("div", { class: "player-row" },
      el("div", { class: "row-main" }, ...main),
      control
    );
  }

  function renderPlayer() {
    const panel = $("#player-panel");
    if (!panel) return;
    const p = state.player;
    const running = !!(state.status && state.status.running);
    if (!p) {
      panel.replaceChildren(el("div", { class: "notice" },
        el("span", {}, "Abre el juego una vez para que cree su archivo de configuración. Después podrás cambiar tu nombre, personaje y colores aquí.")
      ));
      return;
    }
    const look = p.look;
    const disabled = running;

    const tabs = el("div", { class: "toolbar" },
      el("div", { class: "seg", role: "group", "aria-label": "Jugador" },
        el("button", { type: "button", "aria-pressed": "true" }, "Aspecto"),
        el("button", { type: "button", "aria-pressed": "false", onclick: () => {
          toast("Los controles se cambian dentro del juego, en Opciones › Controles.", "ok");
        } }, "Controles")
      )
    );

    const nameInput = el("input", {
      class: "field",
      type: "text",
      maxlength: "20",
      value: look.name,
      disabled,
      "aria-label": "Tu nombre en el juego",
      onchange: (ev) => savePlayerLook({ name: ev.target.value }, false),
    });

    const charSeg = el("div", { class: "char-seg", role: "group", "aria-label": "Personaje" },
      ...p.models.map((name, index) => el("button", {
        type: "button",
        "aria-pressed": String(index === look.model),
        disabled,
        onclick: () => savePlayerLook({ model: index }, true),
      }, name))
    );

    const presets = p.presets || [];
    const chosen = presets.find((preset) =>
      PALETTE_KEYS.every(([key]) => preset.colors[key] === look.colors[key])
    );

    const presetList = el("div", { class: "presets", role: "group", "aria-label": "Presets" },
      ...presets.map((preset) => {
        const button = el("button", {
          class: "preset",
          type: "button",
          "aria-pressed": String(chosen === preset),
          title: preset.name,
          disabled,
          onclick: () => savePlayerLook(
            preset.model === null ? { colors: preset.colors } : { model: preset.model, colors: preset.colors },
            true
          ),
        },
          presetChip(preset.colors),
          el("span", { class: "preset-name" }, preset.name)
        );
        if (preset.builtIn || disabled) return button;
        return el("div", { class: "preset-wrap" },
          button,
          el("button", {
            class: "preset-remove",
            type: "button",
            title: "Borrar preset",
            "aria-label": `Borrar el preset ${preset.name}`,
            onclick: () => deletePlayerPreset(preset.name),
          }, "×")
        );
      }),
      el("button", {
        class: "preset preset-add",
        type: "button",
        disabled: disabled || state.playerBusy,
        "aria-label": "Guardar estos colores como preset",
        onclick: () => { state.playerPresetOpen = true; renderPlayer(); },
      },
        el("span", { class: "preset-chip", "aria-hidden": "true" }),
        el("span", { class: "preset-name" }, "Guardar…")
      )
    );

    const presetBar = state.playerPresetOpen ? (() => {
      const input = el("input", {
        class: "field",
        type: "text",
        maxlength: "30",
        placeholder: "Nombre del preset",
        "aria-label": "Nombre del preset",
      });
      const close = () => { state.playerPresetOpen = false; renderPlayer(); };
      const save = async () => {
        const name = input.value.trim();
        if (!name) { toast("Ponle un nombre al preset.", "bad"); return; }
        try {
          state.player = await postJSON("/api/player/preset", { name });
          state.playerPresetOpen = false;
          renderPlayer();
          toast(`Look "${name}" guardado.`, "ok");
        } catch (err) {
          toast(err.message || "No se pudo guardar el preset.", "bad");
        }
      };
      input.addEventListener("keydown", (ev) => {
        if (ev.key === "Enter") save();
        if (ev.key === "Escape") close();
      });
      requestAnimationFrame(() => input.focus());
      return el("div", { class: "inline-bar" },
        el("span", { class: "label" }, "Nombre del preset"),
        input,
        el("button", { class: "btn", type: "button", onclick: close }, "Cancelar"),
        el("button", { class: "btn btn-primary", type: "button", onclick: save }, "Guardar")
      );
    })() : null;

    const swatches = el("div", { class: "swatches" },
      ...PALETTE_KEYS.map(([key, label]) => {
        const input = el("input", {
          type: "color",
          value: look.colors[key],
          disabled,
          "aria-label": label,
          oninput: (ev) => {
            look.colors[key] = ev.target.value;
            savePlayerColor(key, ev.target.value);
          },
        });
        return el("label", { class: "swatch" }, input, el("span", {}, label));
      })
    );

    panel.replaceChildren(...[
      tabs,
      running ? el("div", { class: "notice" }, el("span", {}, "El juego está abierto. Ciérralo para cambiar tu jugador.")) : null,
      playerGroup("Nombre y personaje", "Cómo te ven los demás jugadores.",
        playerRow([
          el("div", { class: "row-name" }, "Tu nombre"),
          el("div", { class: "row-sub" }, "Aparece sobre tu cabeza en multijugador."),
        ], nameInput),
        playerRow([
          el("div", { class: "row-name" }, "Personaje"),
          el("div", { class: "row-sub" }, "Los modelos del juego. Los mods de Character Select tienen su propio menú."),
        ], charSeg)
      ),
      playerGroup("Colores", "Elige un preset y luego cambia la parte que quieras.",
        presetList,
        presetBar,
        swatches
      )
    ].filter(Boolean));
  }

  async function savePlayerLook(patch, rerender) {
    if (!state.player || state.playerBusy) return;
    state.playerBusy = true;
    const look = { ...state.player.look, ...patch };
    try {
      state.player = await postJSON("/api/player", look);
      if (rerender) renderPlayer();
    } catch (err) {
      toast(err.message || "No se pudo guardar.", "bad");
      await loadPlayer();
    } finally {
      state.playerBusy = false;
    }
  }

  const playerTimers = {};
  function savePlayerColor(key, value) {
    if (!state.player) return;
    clearTimeout(playerTimers[key]);
    playerTimers[key] = setTimeout(async () => {
      try {
        state.player = await postJSON("/api/player", state.player.look);
      } catch (err) {
        toast(err.message || "No se pudo guardar el color.", "bad");
      }
    }, 300);
  }

  async function deletePlayerPreset(name) {
    if (!confirm(`¿Borrar el preset "${name}"? Tus colores actuales se quedan como están.`)) return;
    try {
      state.player = await postJSON("/api/player/preset/delete", { name });
      renderPlayer();
    } catch (err) {
      toast(err.message || "No se pudo borrar el preset.", "bad");
    }
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

  function saveFace(exists, big) {
    return el("span", {
      class: "save-face" + (exists ? "" : " is-new") + (big ? " is-big" : ""),
      "aria-hidden": "true",
    });
  }

  function sm64(text, cls) {
    const node = document.createElement("sm64-text");
    node.setAttribute("text", text);
    if (cls) node.className = cls;
    return node;
  }

  function starCount(stars) {
    if (state.rom && state.rom.ready) {
      return el("span", { class: "save-count", "aria-label": `${stars} estrellas` },
        el("img", { src: "/rom-assets/hud/star.png", alt: "" }),
        sm64(stars < 100 ? `×${stars}` : String(stars))
      );
    }
    return el("span", { class: "save-count", "aria-label": `${stars} estrellas` }, "★ " + stars);
  }

  function renderSaves() {
    const grid = $("#saves-grid");
    const data = state.saves;
    if (!data) return;
    if (state.savePick < 0 || state.savePick >= data.slots.length) state.savePick = 0;
    const list = el("div", { class: "save-list" },
      ...data.slots.map((slot) => {
        const exists = slot.valid && slot.exists;
        const picked = state.savePick === slot.index;
        const custom = (data.names && data.names[slot.index] || "").trim();
        const host = state.status && slot.index === (state.status.hostSlot - 1);
        return el("button", {
          class: "save-row" + (exists ? "" : " is-new") + (picked ? " is-picked" : ""),
          type: "button",
          "aria-pressed": String(picked),
          "aria-label": `${slotLabel(slot.index)}: ${exists ? `${slot.stars} estrellas` : "vacío"}`,
          onclick: () => { state.savePick = slot.index; renderSaves(); },
        },
          saveFace(exists, false),
          el("span", { class: "save-text" },
            el("span", { class: "save-title" }, `MARIO ${SLOT_LETTERS[slot.index]}`),
            el("span", { class: "save-sub" },
              host ? el("span", { class: "save-host" }, "Anfitrión") : null,
              exists && custom ? custom : ""
            )
          ),
          exists ? starCount(slot.stars) : el("span", { class: "save-count" }, sm64("NUEVO", "new-word")),
          el("span", { class: "save-go", "aria-hidden": "true" }, exists ? "▶" : "")
        );
      })
    );

    const slot = data.slots[state.savePick] || data.slots[0];
    const exists = slot.valid && slot.exists;
    const custom = (data.names && data.names[slot.index] || "").trim();
    const host = state.status && slot.index === (state.status.hostSlot - 1);
    const actions = exists
      ? [
          el("button", { class: "btn btn-primary", type: "button", onclick: () => openCopyDialog(slot.index) }, `Copiar MARIO ${SLOT_LETTERS[slot.index]} a…`),
          el("button", { class: "btn", type: "button", onclick: () => clearSlot(slot) }, "Borrar…"),
        ]
      : [
          el("button", { class: "btn", type: "button", onclick: () => $("#save-file").click() }, "Importar una partida…"),
        ];

    const inspector = el("section", { class: "save-inspector", "aria-label": `MARIO ${SLOT_LETTERS[slot.index]}` },
      el("div", { class: "inspect-body" },
        el("div", { class: "inspect-head" },
          saveFace(exists, true),
          el("div", { class: "inspect-title" },
            el("span", { class: "save-title" }, `MARIO ${SLOT_LETTERS[slot.index]}`),
            exists ? starCount(slot.stars) : sm64("NUEVO", "new-word")
          )
        ),
        host && exists ? el("p", { class: "inspect-note" }, "Al ser anfitrión se usa este archivo.") : null,
        exists ? el("p", { class: "inspect-note" },
          `${slot.coins} monedas · gorra nivel ${slot.capLevel}, zona ${slot.capArea}.`) : el("p", { class: "inspect-note" }, "Esta ranura está vacía."),
        exists && custom ? el("p", { class: "inspect-note" }, `Nombre: ${custom}`) : null,
        el("div", { class: "inspect-actions" }, ...actions)
      )
    );

    grid.replaceChildren(el("div", { class: "saves-page" },
      el("section", { class: "group saves-game" },
        el("div", { class: "group-head" },
          el("h2", { class: "group-title" }, "En el juego"),
          el("span", { class: "caption" }, "Los cuatro archivos del juego.")
        ),
        list
      ),
      inspector
    ));
  }

  function openCopyDialog(from) {
    $("#copy-title").textContent = `Copiar ${slotLabel(from)} a…`;
    const targets = $("#copy-targets");
    targets.replaceChildren();
    for (let i = 0; i < 4; i++) {
      if (i === from) continue;
      const occupied = state.saves.slots[i] && state.saves.slots[i].exists;
      targets.append(el("button", {
        class: "btn",
        type: "button",
        onclick: () => { $("#copy-dialog").close(); copySlot(from, i); },
      }, `${slotLabel(i)}${occupied ? " (se reemplaza)" : " (vacío)"}`));
    }
    $("#copy-dialog").showModal();
  }

  async function copySlot(from, to) {
    await guard(async () => {
      await postJSON("/api/saves/copy", { from, to });
      await loadSaves();
      toast(`Se copió ${slotLabel(from)} a ${slotLabel(to)}.`, "ok");
    });
  }

  async function clearSlot(slot) {
    if (!confirm(`¿Borrar ${slotLabel(slot.index)}?\n\nQueda una copia de seguridad junto al archivo.`)) return;
    await guard(async () => {
      await postJSON("/api/saves/clear", { slot: slot.index });
      await loadSaves();
      toast("Ranura borrada.", "ok");
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
      toast("Partida importada. El archivo anterior quedó respaldado.", "ok");
    });
  }

  // ---------- profiles ----------

  async function loadProfiles() {
    state.profiles = await api("/api/profiles");
    renderProfiles();
    renderProfileSelect();
  }

  function renderProfileSelect() {
    const select = $("#profile-select");
    if (!select || !state.profiles) return;
    select.replaceChildren(...(state.profiles.names || []).map((name) =>
      el("option", { value: name, selected: name === state.profiles.active ? "selected" : null },
        name === "Default" ? "Principal" : name)
    ));
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
            el("strong", {}, name === "Default" ? "Principal" : name),
            active ? el("span", { class: "tag pack" }, "activo") : null
          ),
          el("div", { class: "item-id" }, `${count} mod${count === 1 ? "" : "s"}`)
        ),
        el("div", { class: "item-actions" },
          active ? null : el("button", {
            class: "btn",
            type: "button",
            disabled: mutationBlocked(),
            onclick: () => switchProfile(name),
          }, "Usar"),
          active ? null : el("button", {
            class: "btn btn-ghost btn-danger",
            type: "button",
            onclick: () => deleteProfile(name),
          }, "Borrar")
        )
      );
    });
    list.replaceChildren(...rows);
  }

  async function switchProfile(name) {
    await guard(async () => {
      await postJSON("/api/profiles/switch", { name });
      await Promise.all([loadProfiles(), loadMods()]);
      toast(`Ahora usas el perfil ${name}.`, "ok");
    });
  }

  async function deleteProfile(name) {
    if (!confirm(`¿Borrar el perfil "${name}"? Tus mods siguen instalados; solo se va la lista de cuáles están activos.`)) return;
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
      toast("Abriendo el juego…", "ok");
      setTimeout(() => refreshStatus().catch(() => {}), 1500);
    });
  }

  // ---------- tabs ----------

  function showTab(name) {
    state.tab = name;
    setPage(name);
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
    if (name === "copy") guard(async () => {
      await refreshCopy(true);
      const details = $("#copy-log-card");
      if (details && details.open) await refreshCopyLogs();
    });
    if (name === "browse") guard(async () => {
      try { await loadCategories(); } catch { /* the list still works */ }
      if (!state.browse.loaded) await loadBrowse(true);
    });
    if (name === "saves") guard(loadSaves);
    if (name === "player") guard(loadPlayer);
    if (name === "profiles") guard(loadProfiles);
    if (name === "mods") guard(loadMods);
    if (name === "settings") guard(refreshROM);
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
    for (const btn of document.querySelectorAll(".back-to-mods")) {
      btn.addEventListener("click", () => showTab("mods"));
    }
    $("#friends").addEventListener("click", () => showTab("play"));
    $("#open-copy").addEventListener("click", () => showTab("copy"));
    $("#profile-select").addEventListener("change", (ev) => {
      if (ev.target.value) guard(() => switchProfile(ev.target.value));
    });
    $("#profile-manage").addEventListener("click", () => showTab("profiles"));
    for (const btn of document.querySelectorAll("[data-mod-filter]")) {
      btn.addEventListener("click", () => {
        state.modFilter = btn.dataset.modFilter;
        for (const other of document.querySelectorAll("[data-mod-filter]")) {
          other.setAttribute("aria-pressed", String(other === btn));
        }
        renderMods();
      });
    }
    $("#browse-ok").addEventListener("click", () => { $("#browse-notice").hidden = true; });
    $("#rom-file").addEventListener("change", (ev) => {
      const file = ev.target.files && ev.target.files[0];
      ev.target.value = "";
      guard(() => uploadROM(file));
    });
    $("#create-existing").addEventListener("change", (ev) => {
      const file = ev.target.files && ev.target.files[0];
      ev.target.value = "";
      if (!file) return;
      guard(async () => {
        await uploadMod(file, false);
        showTab("mods");
      });
    });
    $("#create-first").addEventListener("click", () => guard(async () => {
      const name = prompt("Nombre del mod nuevo:");
      if (!name || !name.trim()) return;
      const res = await postJSON("/api/create/mod", { name: name.trim() });
      await loadMods();
      showTab("mods");
      toast(`${res.name} creado. Está en tu carpeta de mods, listo para probar.`, "ok");
    }));
    $("#session-log-card").addEventListener("toggle", (ev) => {
      if (ev.target.open) guard(refreshSessionLogs);
    });
    $("#copy-log-card").addEventListener("toggle", (ev) => {
      if (ev.target.open) guard(refreshCopyLogs);
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
    setPage("mods");
    try {
      await refreshStatus();
      refreshROM(true).catch(() => {});
      await Promise.all([loadMods(), loadSaves(), loadProfiles()]);
      await refreshSession(true).catch(() => {});
      renderSession();
      await refreshCopy(true).catch(() => {});
      renderCopy();
    } catch (err) {
      toast(err.message || "Could not talk to the loader.", "bad");
    }
    setInterval(() => refreshStatus().catch(() => {}), 5000);
    setInterval(() => {
      if (state.tab === "play" || state.session) refreshSession().catch(() => {});
    }, 1000);
    setInterval(() => {
      if (state.tab === "copy" || state.copy) refreshCopy().catch(() => {});
    }, 1000);
  }

  boot();
})();
