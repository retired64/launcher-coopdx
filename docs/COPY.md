# Copia entre dispositivos (`copy/v1`)

Este documento explica la fase 4: copiar **mods, aspecto y partidas** entre dos
dispositivos con un código. Es la implementación en Go del canal `copy/v1`
observado en el cliente de escritorio 1.9.0 y en el APK 1.3.0.

No hay que confundirlo con la sincronización de mods al unirse a una partida:
esa la hace el propio juego y está en [MOD_SYNC.md](MOD_SYNC.md). La copia sirve
para **preinstalar** mods en otro dispositivo y para mover el aspecto y las
partidas, no para poder entrar a una sala.

## 1. Idea general

```text
Dispositivo A (send)                       Dispositivo B (get)
────────────────────                       ───────────────────
                                              crea la oferta WebRTC
        ◄──────── ofertas por trackers ─────
responde la oferta
        ──────── respuesta cifrada ────────►
        ◄══════ DataChannel "copy" ════════►
              ordenado y fiable
        ──────── manifiesto JSON ──────────►
        ◄──────── want (índices) ───────────
        ──────── frames binarios ──────────►
        ◄──────── ack / done / stop ────────
```

El receptor es quien **crea la oferta** (`get`). El emisor es quien
**responde** (`send`). Es al revés que en la partida, donde el que se une crea
la oferta.

## 2. Código y criptografía

- Mismo alfabeto Crockford y misma normalización que la partida.
- `PBKDF2-HMAC-SHA-256`, **600 000** iteraciones, `dkLen = 52`.
- Sal distinta:

```text
coop-mod-loader/room/v2   partida
coop-mod-loader/copy/v1   copia
```

```text
infoHash = dk[0:20]    ruta /room/<hex> y anuncio del tracker
key      = dk[20:52]   AES-256-GCM para el SDP
```

El SDP se cifra con AES-256-GCM y se envía como
`Base64(nonce || ciphertext || tag)`. Un SDP manipulado no descifra.

El código es la contraseña del canal. No se publica en logs ni en la UI más allá
de mostrarlo para copiarlo.

## 3. Señalización

Se reutiliza el mismo motor de trackers de la partida:

```text
wss://<relay>/room/<hex(infoHash)>
wss://tracker.openwebtorrent.com
wss://tracker.webtorrent.dev
wss://tracker.novage.com.ua/announce
wss://tracker.peerweb.site/announce
```

- `info_hash` viaja como string binario de 20 bytes (`\u00xx` en JSON).
- `peer_id` y `offer_id` son 20 letras A-Z.
- El receptor publica **6 ofertas** con el mismo SDP cifrado.
- Anuncia cada **2 s** mientras está ofreciendo.
- Espera al otro dispositivo **90 s**.
- El emisor deduplica ofertas por `a=fingerprint` del SDP y contesta una sola vez.
- El receptor acepta una sola respuesta: el primer `offer_id` válido.
- `COOP_RELAY_URL` cambia el Worker; `COOP_NO_TRACKERS=1` desactiva los cuatro
  trackers públicos; `COOP_RELAY_ONLY=1` fuerza TURN.

## 4. ICE

- STUN base: `stun.l.google.com:19302`, `stun.cloudflare.com:3478`.
- `GET <relay>/` con timeout de **4 s** para obtener credenciales TURN.
- Si falla, sigue solo con STUN y registra
  `relay logins unavailable right now; direct routes only`.
- Recolección de candidatos: máximo **5 s**; termina antes al ver un candidato
  `relay` si hay TURN, o `srflx` + **300 ms** si no lo hay.
- `COOP_RELAY_ONLY=1` fuerza `iceTransportPolicy: relay` (pruebas).

La ruta se publica como `direct` o `relay`, igual que en la partida.

## 5. DataChannel

```text
label: "copy"
ordered: true
reliable: true (sin maxRetransmits)
```

El emisor comprueba que el canal del receptor sea `copy` y ordenado; si no,
lo cierra.

### Mensajes de control (texto JSON)

| `t` | Campos | Quién |
|---|---|---|
| `hello` | `v`, `device{name,kind}` | ambos, al abrir |
| `manifest` | `i`, `n`, `text` | emisor → receptor |
| `want` | `files: [índices]` | receptor → emisor |
| `ack` | `bytes` | receptor → emisor |
| `done` | `note` | receptor → emisor |
| `stop` | `why` | cualquiera |

`v` es **1**. Si el otro tiene una versión mayor:
`Update Coop Mod Loader on this device: the other one is newer.` Si es menor:
`Update Coop Mod Loader on <device>: it is older than this one.`

### Mensajes binarios (archivos)

Cabecera de **12 bytes** little-endian:

```text
u32 fileIndex
u32 offsetLow
u32 offsetHigh
datos (hasta 64 KiB)
```

`offset = offsetLow + offsetHigh * 2^32`.

## 6. Manifiesto

JSON compacto, máximo **4 MiB**, troceado en mensajes de hasta **60 000**
caracteres (máximo 70 mensajes). Campo por campo:

```json
{
  "v": 1,
  "device": { "name": "fedora", "kind": "linux" },
  "player": { "name": "Sender", "model": 2, "colors": { "shirt": "#ff8010", "...": "..." } },
  "saveNames": ["Sender Save", null, null, null],
  "presets": [ { "name": "SenderPreset", "colors": { "...": "#rrggbb" }, "model": 1 } ],
  "save": "<base64 de 512 B o null>",
  "savs": [ { "name": "extra.sav", "text": "<Latin-1>" } ],
  "mods": [
    {
      "id": "SenderMod",
      "type": "mod",
      "location": "user",
      "name": "Sender Mod",
      "enabled": true,
      "isFolder": true,
      "record": { "siteId": 12, "siteUrl": "https://…", "title": "…", "version": "1.2", "installedAt": "…" },
      "files": [ { "rel": "main.lua", "size": 42, "sha256": "…", "code": false } ]
    }
  ]
}
```

Reglas de validación (idénticas al cliente observado):

- `device.name` ≤64 sin caracteres de control; `kind` = `windows|linux|android`.
- `player`: nombre ≤20, modelo 0..4, exactamente las 8 claves de color
  `shirt, pants, cap, emblem, gloves, shoes, skin, hair` en `#rrggbb`.
- `saveNames`: máximo 4; cada uno `null` o string ≤32.
- `presets`: máximo 300; nombre ≤30 que pase el saneador; colores; modelo
  `null` o 0..4.
- `save`: `null` o base64 de exactamente 512 bytes.
- `savs`: máximo 2 000; nombre seguro y `.sav`; texto Latin-1 ≤1 MiB sin NUL.
- `mods`: máximo 2 000.
  - `id` ≤200 y nombre de archivo seguro.
  - `type` = `mod|pack`; `location` = `user|builtin`.
  - `builtin` no lleva archivos; `user` sí.
  - ruta relativa ≤400 bytes, ≤16 componentes, cada uno seguro (sin `..`,
    sin `/` ni `\`, sin nombres reservados de Windows).
  - tamaño 0..1 GiB; SHA-256 de 64 hex.
  - sin rutas repetidas (sin distinguir mayúsculas).
  - un mod de un solo archivo debe ser `mod`, `.lua/.luac`, y su único archivo
    debe llamarse igual que el id.
  - total de archivos ≤20 000 y total de bytes ≤16 GiB.

Un `mod` de carpeta se envía con todos sus archivos (no solo los que el juego
carga), igual que el cliente original. Se omiten archivos ocultos, enlaces
simbólicos y nombres no portables.

## 7. Transferencia

Constantes copiadas del cliente:

| Constante | Valor |
|---|---:|
| trozo de archivo | 64 KiB |
| ventana en vuelo | 8 MiB |
| ACK | cada 1 MiB |
| `bufferedAmount` alto | 4 MiB |
| progreso | cada 250 ms |
| presupuesto por relé | 500 MB |

Emisor:

1. Construye el manifiesto y lo envía troceado.
2. Espera `want` con índices de archivos **estrictamente crecientes**.
3. Envía los archivos en ese orden, en trozos de 64 KiB.
4. No adelanta más de 8 MiB sin ACK; espera si el buffer supera 4 MiB.
5. Publica progreso; al recibir `done` termina.

Receptor:

1. Concatena los trozos de manifiesto y lo valida.
2. Llama a la revisión local y espera a que el jugador elija qué copiar.
3. Manda `want` con los índices elegidos.
4. Escribe cada trozo en un archivo temporal de staging, comprobando índice,
   offset y longitud.
5. Calcula SHA-256; si no coincide:
   `A file arrived damaged. Nothing was copied; try again.`
6. Si el primer byte de un `.lua` es `0x1B` (bytecode) y el manifiesto no lo
   declaró como `code`, aborta:
   `A file changed on the way. Nothing was copied.`
7. Manda `ack` cada 1 MiB.
8. Al terminar todos los archivos, aplica la copia, manda `done` y publica el
   resultado.

Si algo se desordena: `The copy got mixed up.` Si el otro corta:
`The other device stopped the copy.`

## 8. Qué se aplica

### Mods y packs

- Solo se escriben mods/packs con `location = user`.
- Destino: `<datos>/mods/<id>` o `<datos>/dynos/packs/<id>`.
- Si el destino existe, se mueve antes a
  `<datos>/removed-by-coop-loader/copy-<fecha>/<tipo>/<id>`.
- Un mod que sea un enlace simbólico se deja como está y se avisa:
  `Left as they are (projects from Create on this computer): …`.
- Se copia la procedencia (`installed.json`) si el manifiesto la trae; si no,
  se olvida la del id.
- Se reemplaza el bloque de `enable-mod:` / `dynos-pack:` por los mods del
  emisor que estén disponibles (copiados o ya presentes). Los mods locales que
  el emisor no tiene quedan desactivados; no se borran.
- Si el jugador desmarca **Mods**, no se toca el bloque de mods activos.

### Aspecto

- `coop_player_name`, `coop_player_model` y las 8 claves
  `coop_player_palette_<color>` en `sm64config.txt`.
- Cada preset se escribe en `<datos>/palettes/<nombre>.ini` con el formato
  `[PALETTE]` y las claves `PANTS_R/G/B`, `SHIRT_*`, etc.
- `<config>/coop-loader/player-presets.json` recuerda el personaje de cada
  preset.

### Partidas

- `sm64_save_file.bin` de 512 bytes, solo si el emisor lo tiene.
- Nombres de slot (`save-name:`) del manifiesto.
- Archivos `.sav` de mods en `<datos>/sav/`, leídos y escritos como Latin-1.
- Si el emisor no tiene partida, **no** se borra la local. Es una diferencia
  deliberada con el cliente original, que movía la local a la papelera.

### Configuración

- Se escribe una sola vez, con copia de seguridad
  `sm64config.modloader-backup.txt` y escritura atómica. Los mods, paletas,
  partidas y `.sav` reemplazados van a
  `removed-by-coop-loader/copy-<fecha>/`.
- No se toca nada si el juego está abierto. El mensaje es:
  `Close the game first. It rewrites its settings when it closes and would undo the copy.`

## 9. Diferencias deliberadas con el cliente original

La red, el manifiesto, los frames y los límites son idénticos. La aplicación de
archivos es más conservadora:

| Tema | Cliente original | Este loader |
|---|---|---|
| Archivos reemplazados | Papelera del sistema | `removed-by-coop-loader/copy-<fecha>/` |
| Emisor sin partida | mueve la partida local | deja la partida local |
| “Mods” desmarcado | podía reescribir el bloque de activos | no toca el bloque |
| Juego abierto al empezar | permitía leer | se niega a empezar |
| Rutas del manifiesto | concatenación directa | validadas, sin `..` ni absolutas |
| Registro inválido | rechaza todo el manifiesto | se omite el registro |
| Troceado del manifiesto | 60 000 unidades UTF-16 | 60 000 runas UTF-8 |

Ninguna de estas diferencias cambia el formato en el cable.

## 10. API

```text
GET  /api/copy/state          {copy: null | State}
POST /api/copy/start          {role: "send"|"get", code}
POST /api/copy/accept         {mods, look, saves}
POST /api/copy/stop           {}
GET  /api/copy/logs           {logs: [...]}
```

Estados: `starting → waiting → connected → review → copying → done`, más
`failed` y `stopped`.

El estado incluye `role`, `code`, `trackers`, `peer{name,kind}`, `route`,
`review`, `progress{done,total}` y `message`.

## 11. UI

Pestaña **Copy**:

- **Send from this computer** genera un código y lo muestra para copiarlo.
- **Get from another device** pide el código.
- Consentimiento la primera vez (IP, trackers, relay, Lua del otro dispositivo).
- Revisión con tres casillas: **Mods**, **Look**, **Saves**.
- Resumen de mods añadidos/actualizados, bytes, cuáles quedarán activos, cuáles
  se desactivan, cuáles no están y cuáles se omiten por el relé.
- Aviso si se van a reemplazar partidas con más estrellas.
- Barra de progreso y botones Stop / Done / Try again.
- Registro de conexión con nombres cortos, ruta y bytes; nunca el código.

## 12. Cómo probarlo

Automático:

```bash
go test ./internal/devicecopy ./internal/app ./internal/server -v
```

Cubre:

- frames de 12 bytes y offsets de 64 bits;
- validación de rutas (`..`, nombres reservados, duplicados);
- plan/revisión y comparación por SHA-256;
- copia completa emisor↔receptor por un relay local;
- archivo vacío y archivo de 9 MiB (ventana y ACK);
- SHA-256, staging y aplicación de mods, config, paleta, partida y `.sav`;
- backups en `removed-by-coop-loader`.

Manual:

1. Abre el loader en dos dispositivos.
2. En uno: **Copy → Send from this computer**.
3. En el otro: **Copy → Get from another device**, escribe el código.
4. Revisa y pulsa **Copy to this computer**.
5. Comprueba mods, aspecto y partidas. Lo reemplazado queda en
   `removed-by-coop-loader`.

## 13. Seguridad

- El código son 40 bits, igual que la partida. No lo publiques.
- El canal va cifrado con AES-GCM para el SDP y DTLS-SRTP para los datos.
- El SHA-256 detecta corrupción y cambios, pero **no autentica** al emisor: el
  código compartido es la única autenticación.
- Copiar mods significa aceptar Lua del otro dispositivo. Copia solo entre
  dispositivos tuyos o de gente de confianza.
- La copia se niega a escribir mientras `sm64coopdx` está abierto.
- No se puede copiar y jugar a la vez: si hay una partida activa, la copia no
  empieza; si hay una copia activa, la partida tampoco.
- Nunca se borra lo reemplazado: se mueve a `removed-by-coop-loader`.
