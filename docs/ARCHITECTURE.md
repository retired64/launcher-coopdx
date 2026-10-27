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
                   ├── internal/session     sala, WebRTC y puente UDP
                   ├── internal/devicecopy  copy/v1: manifiesto y archivos
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
   `sm64config.txt` al salir. La detección ignora procesos zombis (estado `Z`)
   y `game.Launch` recoge al hijo con `Wait` para que no queden zombis.
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
- `SetValue` escribe pares `clave valor` (por ejemplo
  `coop_host_save_slot 3`) sin tocar el resto del archivo;
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
- **Play y Copy no hacen WebRTC en el navegador.** La pestaña solo pinta
  `/api/session/*` y `/api/copy/*`; los sockets y el cifrado viven en Go.

## Sesión P2P

`internal/session` es un motor de red completo, independiente de la UI:

```
code.go     Crockford, PBKDF2-SHA-256 (600 000), AES-256-GCM
signal.go   un WebSocket por punto de encuentro, anuncio y respuestas
peer.go     pion/webrtc: ofertas, respuestas, ICE, rutas y estadísticas
engine.go   máquina de estados, heartbeats, reconexión y límites
bridge.go   sockets UDP loopback, STILL_HERE y caché de estado
types.go    lo que ve la API (modo, fase, amigos, ruta)
```

Por qué vive en Go y no en el navegador:

- no hace falta una ventana oculta ni mandar cada datagrama por IPC;
- el proceso sobrevive a cerrar o recargar la pestaña;
- el estado se prueba con un relay local y sockets UDP falsos.

Flujo:

```
StartSession(host|join, código)
   │
   ├─ Derive → infoHash + clave
   ├─ Bridge.Start → puerto loopback (join) o 7777 (host)
   ├─ Signal.Start → relay + 4 trackers
   └─ ticker 250 ms
          ├─ ping 500 ms por amigo
          ├─ silencio 2 s → lost + STILL_HERE
          ├─ ICE restart a los 6 s
          ├─ conexión nueva a los 10 s (tras 2 intentos)
          └─ 60 s sin ruta → drop
```

`app.Service` expone `StartSession`, `StopSession`, `SessionState`,
`LaunchSession` y `SessionLogs`; el servidor traduce eso a
`/api/session/state|start|stop|launch|logs`. La UI solo pinta el estado: no
tiene WebRTC ni conoce el código más allá de mostrarlo.

El host escribe `coop_host_save_slot` y `coop_host_port` en `sm64config.txt`
antes de lanzar el juego. El join pasa el puerto efímero del puente con
`--client 127.0.0.1 <puerto>`.

El loader **no** copia mods al unirse. El juego del anfitrión envía su lista de
mods activos y el del amigo descarga los archivos que no tenga en
`.tmp/<sesión>/`, comparando MD5 contra `mod.cache`. El puente solo transporta
esos paquetes; no los interpreta. Está documentado en [MOD_SYNC.md](MOD_SYNC.md).
La fase 4 copia mods/aspecto/partidas entre dispositivos para **preinstalar**,
no porque haga falta para entrar a una partida.

Los detalles byte a byte, los temporizadores y la comparación con el cliente de
referencia están en [SESSION.md](SESSION.md); el contrato de red, en
[PROTOCOL.md](PROTOCOL.md).

## Copia entre dispositivos

`internal/devicecopy` es un motor aparte, aunque reutiliza la señalización y la
criptografía de `internal/session`:

```
types.go     roles, fases, review y límites
wire.go      frames de 12 bytes, troceado y nombres seguros
manifest.go  manifiesto JSON, validación y estrellas de la partida
plan.go      comparación por SHA-256, selección y presupuesto del relé
engine.go    oferta/respuesta, DataChannel "copy", ventana y ACK
```

Diferencias con la partida:

- El **receptor** (`get`) crea la oferta; el **emisor** (`send`) responde.
- El DataChannel se llama `copy` y es **ordenado y fiable**.
- Hay un `hello` con `v:1` y el nombre/tipo de dispositivo.
- El emisor manda un manifiesto JSON troceado; el receptor elige qué copiar.
- Los archivos viajan en frames de 64 KiB con SHA-256 por archivo.

La copia **no** participa en la partida. El juego sincroniza sus mods por su
cuenta (ver [MOD_SYNC.md](MOD_SYNC.md)); `copy/v1` sirve para preinstalar mods y
mover aspecto y partidas.

`app.Service` expone `StartCopy`, `StopCopy`, `CopyState`, `CopyAccept` y
`CopyLogs`; el servidor los traduce a `/api/copy/state|start|accept|stop|logs`.
Al aplicar, el servicio respeta las mismas garantías que el resto:

- se niega mientras el juego está abierto;
- escribe `sm64config.txt` una sola vez, con copia y escritura atómica;
- mueve lo reemplazado a `removed-by-coop-loader/copy-<fecha>/`;
- valida rutas (sin `..`, sin absolutas, sin enlaces simbólicos);
- verifica SHA-256 antes de aplicar y aborta todo si un archivo falla.

El protocolo completo está en [COPY.md](COPY.md).

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
| Sesión en Go, no en el WebView | sin ventana oculta, sin IPC por datagrama, testeable |
| Mismos temporizadores y bytes que el cliente observado | interoperar con la app existente, no inventar una variante |
| Código de 40 bits con aviso explícito | es lo que usa la versión 2; la v3 propia subirá a 128 bits + PAKE |
| STILL_HERE y caché de 2 MiB | un corte corto no debe expulsar al jugador |
| Copia con staging y SHA-256 | un archivo dañado no debe dejar media copia aplicada |
| Lo reemplazado va a `removed-by-coop-loader` | nunca se borran datos del usuario |
