# Protocolo observado (para interoperar)

Este documento describe el **contrato de red y de archivos** que usa la familia
de clientes Coop Mod Loader (Android 1.3.0 y escritorio 1.9.0), obtenido por
análisis estático del artefacto publicado. Sirve para implementar un cliente
compatible **de forma independiente**.

> No es código del proyecto original y no incluye recursos del juego.
> Los endpoints públicos se listan solo como referencia: para producción usa
> **tu propia** infraestructura (Worker/relay y TURN con credenciales firmadas).

## 1. Código de sala

- Alfabeto Crockford: `0123456789ABCDEFGHJKMNPQRSTVWXYZ`.
- 8 símbolos aleatorios uniformes (40 bits), mostrados `ABCD-1234`.
- Normalización al introducirlo: mayúsculas, quitar espacios y guiones,
  `O→0`, `I/L→1`, `U→V`; debe quedar `^[0-9A-HJKMNP-TV-Z]{8}$`.

## 2. Derivación

```
PBKDF2-HMAC-SHA-256(
    password = código normalizado (8 símbolos, sin guion),
    salt     = "coop-mod-loader/room/v2",     // copia: "coop-mod-loader/copy/v1"
    iterations = 600000,
    dkLen    = 52 bytes
)
  infoHash = dk[0:20]      // nombre de sala
  key      = dk[20:52]     // AES-256-GCM
```

Los SDP se cifran con AES-GCM, IV aleatorio de 12 bytes, y se envían
`Base64(IV || ciphertext || tag)`.

## 3. Señalización

WebSockets simultáneos (el cliente de referencia abre cinco):

```
wss://<tu-relay>/room/<hex(infoHash)>
wss://tracker.openwebtorrent.com
wss://tracker.webtorrent.dev
wss://tracker.novage.com.ua/announce
wss://tracker.peerweb.site/announce
```

El `info_hash` se envía como **string binario de 20 bytes** (no hex); el hex solo
va en la ruta del relay.

Oferta (quien se une):

```json
{
  "action": "announce",
  "info_hash": "<20 bytes>",
  "peer_id": "<20 letras A-Z>",
  "numwant": 6,
  "uploaded": 0,
  "downloaded": 0,
  "left": 1,
  "event": "started",
  "offers": [
    { "offer": { "type": "offer", "sdp": "<AES-GCM b64>" }, "offer_id": "<20 letras>" }
  ]
}
```

Respuesta (host):

```json
{
  "action": "announce",
  "info_hash": "<20 bytes>",
  "peer_id": "<host>",
  "to_peer_id": "<joiner>",
  "answer": { "type": "answer", "sdp": "<AES-GCM b64>" },
  "offer_id": "<el recibido>"
}
```

- 6 ofertas por anuncio, con el **mismo** SDP cifrado y distinto `offer_id`.
- Reanuncio cada 15 s; mientras hay ofertas activas, cada 2 s.
- Reconexión del WebSocket con backoff 1 s → 10 s.
- El host deduplica por `a=fingerprint` + `a=ice-ufrag` del SDP y responde una
  sola vez por clave.

## 4. ICE

- STUN base: `stun:stun.l.google.com:19302`, `stun:stun.cloudflare.com:3478`.
- `GET https://<tu-relay>/` → `{ "iceServers": [ { "urls": [...], "username": "...", "credential": "..." } ] }`
  con timeout de 4 s y caché de 30 min. Si falla, solo rutas directas.
- Recolección: termina al ver un candidato `relay`; si no se espera relé, al ver
  `srflx` + 300 ms; tope 3–5 s.

## 5. Canal de juego

`RTCPeerConnection.createDataChannel("game", { ordered: false, maxRetransmits: 0 })`.

Primer byte de cada mensaje:

| Byte | Significado |
|---:|---|
| `1` | hola. Byte 1 = versión (`2`) |
| `2` | ping. Bytes 1–4 = timestamp `uint32 LE`. Byte 5 = juego dentro (0/1) |
| `3` | pong. Bytes 1–4 = eco del timestamp |
| `4` | me voy |
| `120` | datagrama del juego (se reenvía sin tocar) |

Límites del cliente de referencia:

```
paquete máximo ......... 4096 B
bufferedAmount ......... descartar si ≥ 64 KiB
ping ................... 500 ms
silencio = caído ....... 2 s
recuperar ruta ......... 60 s
timeout de unión ....... 90 s
amigos por host ........ 15
resumen de stats ....... cada 60 s
```

## 6. Puente UDP

- **Host:** un socket UDP por amigo, `bind(0, 127.0.0.1)`. Lo que llega del
  juego (`127.0.0.1:7777`) se atribuye al peer por el **puerto de origen** del
  socket; lo que llega del peer se envía a `127.0.0.1:7777`.
- **Join:** un socket `bind(0, 127.0.0.1)` cuyo puerto se pasa al juego
  (`--client 127.0.0.1 <puerto>`). El primer datagrama del juego fija la
  dirección de destino; el resto es simétrico.

El juego distingue jugadores por dirección/puerto de origen: por eso cada amigo
necesita su propio puerto loopback.

## 7. Retención del lugar

Al perder el enlace, se envía al juego cada 2 s (hasta 60 s) un paquete
comprimido con `deflate`:

```
payload = 0F 00 00 00 FF + checksum(~XOR32 de los 5 bytes, LE)   // 9 bytes
```

Además se guardan hasta 2 MiB de los últimos paquetes de estado de ese jugador:
se descomprimen y se indexan por el `uint16 LE` del **offset 1**. Al volver el
enlace se reenvían (32 KiB cada 50 ms).

## 8. Copia entre dispositivos

> Esto **no** es lo que sincroniza los mods al unirse. Eso lo hace el propio
> juego del anfitrión (lista de mods activos, MD5 y descarga a `.tmp/<sesión>/`;
> ver [MOD_SYNC.md](MOD_SYNC.md)). Este canal sirve para copiar entre
> dispositivos: preinstalar mods, aspecto del jugador y partidas.

Misma señalización, sal `coop-mod-loader/copy/v1` (los `infoHash` de copia y de
juego nunca colisionan). DataChannel **`copy` ordenado y fiable**.

Mensajes de control (JSON):

| `t` | Campos | Quién |
|---|---|---|
| `hello` | `v`, `device{name,kind}` | ambos |
| `manifest` | `i`, `n`, `text` | emisor → receptor |
| `want` | `files: [índices]` | receptor → emisor |
| `ack` | `bytes` | receptor → emisor |
| `done` | `note` | receptor → emisor |
| `stop` | `why` | cualquiera |

- Manifiesto: JSON troceado en mensajes de 60 000 caracteres, máximo 4 MiB.
- Archivos: frames binarios `u32 fileIndex | u32 offsetLo | u32 offsetHi | datos`,
  datos ≤ 64 KiB.
- Ventana: máximo 8 MiB en vuelo; ACK cada 1 MiB; esperar si
  `bufferedAmount > 4 MiB`.
- Verificación: SHA-256 por archivo; si uno no coincide se aborta **todo**.
- Límites: 2 000 mods, 16 GiB total, 90 s para encontrar al otro dispositivo.
- Por relé se omiten los mods que no caben en 500 MB.

Manifiesto (campos observados):

```json
{
  "v": 1,
  "device": { "name": "…", "kind": "windows|linux|android" },
  "player": { "name": "…", "model": 0, "colors": { "shirt": "#rrggbb", "...": "..." } },
  "saveNames": ["…", null, null, null],
  "presets": [ { "name": "…", "colors": {}, "model": 0 } ],
  "save": "<base64 de 512 B o null>",
  "savs": [ { "name": "algo.sav", "text": "…" } ],
  "mods": [
    {
      "id": "…", "type": "mod|pack", "location": "user",
      "isFolder": true,
      "files": [ { "rel": "main.lua", "size": 123, "sha256": "…" } ],
      "record": { "siteId": 1, "siteUrl": "…", "title": "…", "version": "…", "installedAt": "…" }
    }
  ]
}
```

`record` es opcional (procedencia en el sitio de mods).

## 9. Formato de partida

`sm64_save_file.bin`, 512 bytes, 4 slots de 112:

| Offset | Bytes | Contenido |
|---|---|---|
| 0 | 1 | nivel de gorra |
| 1 | 1 | área de gorra |
| 2 | 6 | posición XYZ (`int16`) |
| 8 | 4 | flags (`bit 0` = existe) |
| 12 | 25 | estrellas (bitmap) |
| 37 | 15 | monedas |
| 52 | 2 | `"DA"` = big-endian, `"AD"` = little-endian |
| 54 | 2 | checksum = suma de los 54 bytes anteriores |
| 56 | 56 | datos extra |
| 448 | 64 | 2 bloques de 32 B (firma `"HI"` en +28) |

## 10. Compatibilidad y versiones

Para hablar con los clientes actuales hay que clavar los puntos 1–9. Si vas a
diseñar tu propia versión:

- **Sube la entropía del código.** 40 bits y una clave derivada del código
  permiten fuerza bruta offline desde el `infoHash` publicado. Usa 128 bits de
  clave aleatoria y un PAKE (SPAKE2/OPAQUE) para emparejar.
- Versiona la sala (`room/v3`) y la copia (`copy/v2`), y firma el manifiesto.
- Mantén un adaptador `v2` solo si necesitas interoperar con la app existente.

## 11. Notas de implementación (Go)

Detalles que no se ven en el JSON pero que hay que clavar para interoperar:

- **`info_hash` en JSON.** Los 20 bytes se emiten como 20 escapes `\u00xx`
  (cada byte = un code point). `encoding/json` con `[]byte` los convertiría a
  Base64 y rompería la sala. En Go: `binString` en `internal/session/signal.go`.
- **`120` es `0x78`.** Los datagramas del juego son streams `zlib`, y su primer
  byte es 120. Por eso el protocolo usa 120 como marca de "paquete de juego" y
  no hace falta un campo de tipo adicional.
- **Join de escritorio.** El cliente de escritorio enlaza un puerto efímero
  (`bind(0, 127.0.0.1)`) y se lo pasa al juego con `--client`; el APK intenta
  primero 7777 y cae a efímero si está ocupado.
- **Host.** No abre el 7777: lo abre el juego. Crea un socket por amigo y
  envía a `127.0.0.1:7777`; el juego ve una dirección de origen distinta por
  jugador.
- **Antes de lanzar como host.** El cliente escribe `coop_host_save_slot` y
  `coop_host_port` en `sm64config.txt`; son líneas planas, no una sección.
- **Un solo proceso.** Este port corre la señalización y el puente en el
  proceso del loader. No hay ventana auxiliar de Electron, así que no existe el
  "resume" de un renderer que se cae; el estado sobrevive a recargar la UI.
- **Relay propio.** `https://coop-mod-loader-relay.jehu-cl.workers.dev` es el
  endpoint de referencia, no infraestructura nuestra. Para producción,
  `COOP_RELAY_URL` debe apuntar a un Worker propio con credenciales TURN
  efímeras.

## 12. Desconocidos

- Implementación, almacenamiento, TTL y rate-limit del Worker del relay.
- Proveedor TURN real y duración de las credenciales.
- Comportamiento del relay bajo carga o con IPs bloqueadas.
- Lo que los trackers públicos registren.

Nada de esto se puede afirmar desde el cliente: son decisiones del servidor.
