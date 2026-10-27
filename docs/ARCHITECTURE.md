# Arquitectura

## Principio

El núcleo no depende de la interfaz. `internal/app` es la única puerta a las
operaciones que tocan archivos del juego; el CLI y el servidor HTTP son dos
adaptadores finos sobre él. Eso permite añadir mañana una ventana nativa
(Wails), una app Android con `gomobile` o una API para scripts sin duplicar
reglas.

```
cmd/coop-loader        flags, salida por consola, servidor
   │
   ├── internal/server  HTTP + UI embebida + token
   │       │
   └───────┴──► internal/app  (Service)
                   ├── internal/settings
                   ├── internal/game
                   ├── internal/catalog     sitio de mods: HTML, caché, descargas
                   ├── internal/deps        globales Lua: quién provee / qué falta
                   ├── internal/installed   procedencia de lo instalado
                   ├── internal/conf        sm64config.txt + perfiles
                   ├── internal/mods        listar, instalar, quitar
                   └── internal/eeprom      partida 512 B
                            │
                            └── internal/fsutil (escritura atómica, SafeJoin)
```

## Invariantes de seguridad

Toda operación que modifica el juego pasa por estas reglas:

1. **`game.Running()` primero.** Si el proceso `sm64coopdx` está vivo, se
   devuelve `ErrGameRunning` y no se toca nada. El juego reescribe
   `sm64config.txt` al salir.
2. **Respaldo antes de la primera escritura.** `sm64config.txt` se copia a
   `sm64config.modloader-backup.txt` y `sm64config.modloader-original.txt`.
   `sm64_save_file.bin` se copia a `sm64_save_file.modloader-backup.bin`.
3. **Escritura atómica.** `fsutil.WriteFileAtomic` escribe en un temporal en el
   mismo directorio, hace `fsync` y renombra. Un corte deja el archivo viejo o
   el nuevo, nunca uno a medias.
4. **Nada se borra.** Los mods retirados se mueven a
   `<datos>/removed-by-coop-loader/<tipo>/`.
5. **Las rutas del usuario se validan.** `fsutil.SafeJoin` rechaza `..`,
   absolutas y letras de unidad; el instalador de zips valida cada entrada antes
   de escribir y limita a 20 000 entradas / 2 GiB.

## `sm64config.txt`

El juego no tiene base de datos. El estado de los mods son líneas:

```
enable-mod: character-select-coop
dynos-pack: some-model true
save-name: 0 Mario
```

`internal/conf`:

- detecta CRLF/LF y lo conserva;
- reemplaza el bloque completo de `enable-mod:` / `dynos-pack:` en vez de
  insertar líneas sueltas, para que el orden sea estable;
- deduplica y descarta nombres con saltos de línea;
- no toca ninguna otra línea.

Cambiar un mod es `Doc.SetMod(id, on)`, que reconstruye el bloque. Los perfiles
son `Doc.SetMods(set)`.

## Partida (`sm64_save_file.bin`)

512 bytes = 4 slots de 112. Cada slot tiene:

| Offset | Bytes | Contenido |
|---|---|---|
| 0 | 1 | nivel de gorra |
| 1 | 1 | área de gorra |
| 2 | 6 | posición XYZ (`int16`) |
| 8 | 4 | flags (bit 0 = existe) |
| 12 | 25 | estrellas (bitmap) |
| 37 | 15 | monedas por curso |
| 52 | 2 | firma `"DA"` (big-endian) o `"AD"` (little-endian) |
| 54 | 2 | checksum: suma de los 54 bytes anteriores |

`internal/eeprom` detecta el orden de bytes por slot, valida el checksum, cuenta
estrellas con `math/bits`, y expone copiar/borrar/reparar. Nunca escribe un slot
cuya firma y checksum no cuadren (`SetSlot` valida antes de copiar).

## Instalación de mods

`internal/mods/install.go` no extrae el zip a ciegas. Primero construye el plan:

1. ¿Hay carpetas `mods/`, `dynos/packs/` o `palettes/` en los primeros niveles?
   Cada hijo es una unidad.
2. Si no, ¿hay carpetas con `main.lua`? Cada una (la más externa) es un mod.
3. Si no, cada `.lua` suelto es un mod.
4. Si no, `.bin`/`.tex` en la raíz es un pack DynOS.

Después escribe solo las entradas de cada unidad, con `SafeJoin`, presupuesto de
bytes y comprobación de Lua compilado (primer byte `0x1b`) antes de aceptar.

Un nombre destino se valida con `safeName` (sin separadores, sin nombres
reservados de Windows, sin punto/espacio final).

## Catálogo del sitio

El sitio es un foro XenForo sin API JSON. `internal/catalog` hace de cliente
educado:

- **Una caché** de una hora en `<config>/coop-loader/site-cache.json`, con tope
  de 120 páginas. Se guarda al cerrar el proceso (el servidor captura SIGINT y
  SIGTERM para que el `defer` corra).
- **800 ms entre peticiones** al sitio; las visitas a caché no esperan.
- **Solo el origin del sitio** para páginas y ficheros. Las redirecciones se
  siguen a mano, comprobando cada salto.
- **Descargas**: el botón *Download* puede devolver un adjunto, una página con
  varios ficheros o una redirección. Si va a GitHub se resuelve la release con
  la API y se descarga el `.zip` (máximo 1 GiB). Cualquier otro host se devuelve
  como `external` y la UI ofrece abrirlo en el navegador: el loader no descarga
  desde sitios arbitrarios.
- **Procedencia**: `<config>/coop-loader/installed.json` guarda, por id de mod,
  `siteId`, `siteUrl`, `title`, `version` e `installedAt`. La lista de mods
  enseña la versión de origen y Browse marca *Instalado* o *Actualizar*.

El parser (`parse.go`) extrae listados, búsqueda, categorías, ficha y cuerpo
enriquecido (párrafos, títulos, listas, citas, imágenes, enlaces, negritas…).
Las imágenes del cuerpo usan el proxy del propio sitio (`/proxy.php?...`), que
es lo que permite mostrarlas con una CSP estricta.

## Dependencias entre mods

`internal/deps` no ejecuta nada. Quita comentarios y cadenas con una máscara,
busca:

- **proveedores**: `_G.X = ...` y `_G["X"] = ...`;
- **requisitos**: `not _G.X`, `_G.X == nil`, `nil == _G.X` (y sus variantes con
  corchetes), pero solo si el `if` que los contiene **retorna** o avisa al
  jugador. Eso evita marcar comprobaciones inocentes.

Con eso, al instalar:

1. se escanea el Lua del mod nuevo (zip o carpeta, sin pasar de 64 MiB);
2. se indexan los globales que proveen los mods instalados;
3. cada requisito que ningún mod **activo** provee se convierte en una
   sugerencia:
   - `Local`: activar un mod ya instalado que lo provee;
   - `Site`: instalar la ficha enlazada o encontrada por búsqueda, emparejando
     el nombre del global con el título (`characterSelectExists` →
     `Character Select`).

Es heurístico, y así se documenta en la UI: es una ayuda, no una garantía.

## UI

- `internal/server/web` se embebe con `//go:embed`; no hay build de frontend.
- El servidor escucha en loopback y exige `X-Token` (o `?token=`) en `/api/*`.
  El token es aleatorio por ejecución y se imprime en la URL.
- Valida `Host` (`127.0.0.1`, `localhost`, `::1`) contra DNS rebinding.
- CSP estricta: la página solo carga sus propios archivos; no hay CDNs.
- La UI no guarda estado: cada acción llama a la API y vuelve a leer.

## Añadir la sesión P2P (fase 3)

El diseño previsto, para que encaje sin reescribir el núcleo:

```
internal/session
    code.go        normalización + PBKDF2 + AES-GCM
    signal.go      WebSocket tracker (mismo JSON que los trackers públicos)
    peer.go        pion/webrtc, DataChannel "game", ICE restart, spare
    bridge.go      sockets UDP 127.0.0.1:7777, un puerto por peer
    keepalive.go   paquete STILL_HERE + caché de 2 MiB
```

`app.Service` gana `StartSession(mode, code)`, `SessionState()`,
`StopSession()`; el servidor expone `/api/session/*`; la UI muestra el código y
los amigos. Todo el estado de red vive en Go, no en el navegador: así se evita
la ventana oculta y el bombeo de datagramas por IPC que usa Electron.

El contrato de interoperabilidad está en [PROTOCOL.md](PROTOCOL.md).

## Decisiones y porqués

| Decisión | Motivo |
|---|---|
| Núcleo sin dependencias; `goquery` solo en el catálogo | compila sin cgo; el scraping queda aislado en un paquete |
| `embed` para la UI | el binario es todo el programa; no hay rutas relativas |
| Token por ejecución | cualquier web podría hacer POST a `localhost` sin él |
| Respaldo + atómico + papelera | los datos del jugador son irrecuperables si se pierden |
| Validar `main.lua` y checksum | el juego no valida lo que el loader escribe |
| Sin borrar mods | un clic equivocado no debe costar una descarga de 300 MB |
| Caché y pausa de 800 ms | no castigar el sitio con decenas de peticiones |
| Descargar solo del sitio o GitHub | un enlace de un mod no debe poder traer un binario de cualquier host |
| Dependencias por escaneo, no por ejecución | el loader no corre código de mods fuera del juego |
