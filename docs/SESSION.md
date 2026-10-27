# Motor de sesión P2P (`internal/session`)

Este documento explica cómo funciona la fase 3: crear una partida, unirse con
un código, transportar los datagramas de SM64 Coop Deluxe y sobrevivir a un
corte. Es la implementación en Go del motor observado en el cliente de
escritorio 1.9.0 (Electron) y en el APK 1.3.0.

La regla de oro: **el juego sigue creyendo que habla por UDP con un amigo en
`127.0.0.1`**. Todo lo demás (trackers, WebRTC, TURN, cifrado) es invisible para
él.

```text
SM64CoopDX ──UDP──► puente loopback ──► DataChannel "game" ──► WebRTC
  127.0.0.1:7777       (Go)              sin orden ni reintentos      │
                                                                     ▼
SM64CoopDX ◄──UDP── puente loopback ◄── DataChannel "game" ◄── WebRTC del amigo
```

## 1. Código de sala

- Alfabeto Crockford: `0123456789ABCDEFGHJKMNPQRSTVWXYZ`.
- Se generan 8 bytes aleatorios y cada uno se reduce módulo 32. Como 256 es
  múltiplo de 32, el resultado es uniforme: **40 bits**.
- Se muestra `ABCD-1234`.
- Al escribirlo se acepta cualquier mezcla de mayúsculas/minúsculas, espacios,
  guiones y guiones bajos. Después:
  - `O → 0`
  - `I → 1`, `L → 1`
  - `U → V`
  - se exige `^[0-9A-HJKMNP-TV-Z]{8}$`.

```go
code, _ := session.Generate()          // "K7QD-9XPA"
norm, err := session.Normalize("k7qd 9xpa") // "K7QD-9XPA"
```

El código **es la contraseña de la sala**. No codifica una IP, un puerto ni una
identidad. Quien lo tenga puede entrar.

## 2. Derivación criptográfica

```text
PBKDF2-HMAC-SHA-256(
  password   = código sin guion ("K7QD9XPA"),
  salt       = "coop-mod-loader/room/v2",
  iterations = 600000,
  dkLen      = 52 bytes
)

infoHash = dk[0:20]     // identificador binario de la sala
key      = dk[20:52]    // clave AES-256-GCM
```

Los SDP (ofertas y respuestas) se cifran con AES-256-GCM:

```text
nonce = 12 bytes aleatorios
salida = Base64(nonce || ciphertext || tag)
```

El tracker solo ve el `infoHash`, IDs efímeros y SDP cifrados. No ve el código
ni la clave. Un SDP manipulado no descifra: `Room.Open` devuelve `ok=false`.

El vector de prueba está en `code_test.go` y se generó aparte con
`hashlib.pbkdf2_hmac` de Python:

```text
código   ABCD-1234
infoHash 42568f8e201a83e8de74ed167204a49509f4b6e9
clave    f167b2c34eda1e9f866fa7c2e00784b370a563f75faaf7f526b7fff3506e66b4
```

## 3. Puntos de encuentro (señalización)

El motor abre un WebSocket por cada punto:

```text
wss://<relay>/room/<hex(infoHash)>        (por defecto el Worker de referencia)
wss://tracker.openwebtorrent.com
wss://tracker.webtorrent.dev
wss://tracker.novage.com.ua/announce
wss://tracker.peerweb.site/announce
```

`info_hash` viaja como **string binario de 20 bytes**, no como hex. En Go se
emite cada byte como `\u00xx` para que `JSON.parse` del otro lado produzca
exactamente los mismos code points. El hex solo se usa en la ruta del relay.

`peer_id` son 20 letras `A-Z`. Los `offer_id` también son 20 letras aleatorias.

Anuncio:

```json
{
  "action": "announce",
  "info_hash": "<20 bytes>",
  "peer_id": "<20 letras>",
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

Respuesta del host:

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

Reglas:

- **6 ofertas** por publicación, todas con el mismo SDP cifrado y distinto
  `offer_id`.
- Anuncio cada **15 s**; mientras hay ofertas activas, cada **2 s**.
- Al conectar se anuncia de inmediato. Un socket que se cae reconecta con
  backoff **1 s → 2 s → 4 s → 8 s → 10 s**.
- Al recibir una respuesta válida se dejan de anunciar ofertas
  (`SetOffers(nil, false)`).
- El host deduplica por `a=fingerprint` + `a=ice-ufrag` del SDP. Si llega la
  misma oferta por cinco trackers, contesta una sola vez; las demás reciben la
  respuesta ya cifrada desde caché.
- Un `offer_id` repetido con un SDP distinto se ignora.

## 4. ICE, STUN y TURN

STUN base:

```text
stun:stun.l.google.com:19302
stun:stun.cloudflare.com:3478
```

Antes de crear una conexión se hace:

```http
GET https://<relay>/
```

con timeout de **4 s**, esperando:

```json
{ "iceServers": [ { "urls": ["turn:..."], "username": "...", "credential": "..." } ] }
```

El resultado se cachea **30 minutos**. Si falla, se sigue solo con STUN y se
registra `relay logins unavailable right now; direct routes only`.

Recolección de candidatos:

- termina al ver un candidato `relay` si hay TURN;
- si no hay relé, al ver `srflx` + **300 ms**;
- tope **5 s** en la primera conexión y **3 s** en una renovación;
- `RelayOnly` fuerza `iceTransportPolicy: relay` (pruebas y redes restrictivas).

La ruta se clasifica como `direct` o `relay` mirando el par de candidatos
seleccionado. La UI la muestra por amigo.

## 5. DataChannel de juego

```go
ordered := false
maxRetransmits := uint16(0)
dc, _ := pc.CreateDataChannel("game", &webrtc.DataChannelInit{
    Ordered: &ordered, MaxRetransmits: &maxRetransmits,
})
```

Es decir: semántica UDP, sin orden y sin retransmisiones.

Primer byte de cada mensaje:

| Byte | Significado |
|---:|---|
| `1` | hola. Byte 1 = versión (`2`) |
| `2` | ping. Bytes 1–4 = timestamp `uint32 LE`; byte 5 = juego dentro (0/1) |
| `3` | pong. Bytes 1–4 = eco del timestamp |
| `4` | me voy |
| `120` | datagrama del juego, reenviado sin tocar |

> **¿Por qué 120?** Los datagramas del juego son streams `zlib`, y `zlib`
> empieza con `0x78` = 120. El protocolo usa ese byte como marca de "esto es
> juego". Si un paquete no empieza por 120 y no es un control, se ignora.

Límites y temporizadores:

| Constante | Valor |
|---|---:|
| paquete máximo | 4096 B |
| `bufferedAmount` máximo | 64 KiB |
| tick interno | 250 ms |
| ping | 500 ms |
| silencio = caído | 2 s |
| ventana para recuperar ruta | 60 s |
| timeout para encontrar al host | 90 s |
| amigos por host | 15 |
| estadísticas en el log | 60 s |

El ping incluye si el juego local ya está dentro. Al cambiar ese estado se
actualiza la lista de amigos.

## 6. Puente UDP

### Host

El juego escucha en `127.0.0.1:7777` (lo abre el propio juego).

Por cada amigo se crea **un socket UDP independiente** en `127.0.0.1:0`:

- lo que llega del juego a ese socket (origen `127.0.0.1:7777`) se atribuye a
  ese amigo y se manda por su DataChannel;
- lo que llega del amigo se envía **desde ese socket** a `127.0.0.1:7777`.

El juego distingue jugadores por dirección/puerto de origen; por eso cada amigo
necesita su propio puerto loopback. El socket se abre en cuanto se acepta la
oferta, no cuando el canal termina de abrir, para no perder el primer paquete.

### Join

Se abre **un** socket en `127.0.0.1:0` cuyo puerto se pasa al juego:

```text
--client 127.0.0.1 <puerto efímero>
```

El primer datagrama que llega del juego fija `gameAddr` con su dirección y
puerto de origen. Desde entonces el puente es simétrico. Solo se acepta tráfico
desde loopback.

> El cliente de escritorio usa un puerto efímero también al unirse; el APK
> intenta primero 7777. Nuestro join usa efímero, que evita chocar con un juego
> ya abierto.

## 7. Retención del lugar (`STILL_HERE`)

Cuando el heartbeat deja de llegar, el motor marca al amigo como `reconnecting`
y llama al puente con `SetLink(id, false)`.

El puente envía al juego, cada **2 s** y durante **60 s**, este paquete:

```text
payload = 0F 00 00 00 FF 0F FF FF FF
          └──── 5 bytes ────┘ └ checksum ┘
checksum = ~XOR32(payload) en little-endian
```

El payload se comprime con `zlib` (deflate). El juego lo interpreta como "sigo
aquí, no me expulses".

Mientras el enlace está caído se guardan los últimos paquetes de estado:

- se descomprime cada paquete (máximo 4096 B de salida);
- se usa el `uint16 LE` del **offset 1** como clave;
- si la clave se repite, se reemplaza el paquete;
- tope **2 MiB**; al pasarse se eliminan los más antiguos;
- al volver el enlace se reenvían en orden, **32 KiB cada 50 ms**.

Esto explica por qué un corte corto no expulsa al personaje.

## 8. Máquina de estados y reconexión

Fases públicas:

```text
starting → waiting (host) | connecting (join) → connected → failed | stopped
```

Algoritmo de recuperación:

1. Si el enlace lleva **2 s** sin heartbeat, se marca `lostAt`.
2. El join intenta primero un **ICE restart** (rollback + nueva oferta con
   `iceRestart`), esperando 3 s de recolección.
3. Si el enlace no existe o ya hubo **2 intentos**, construye una **conexión
   nueva** en paralelo (`spare`), con un DataChannel nuevo.
4. Espera **6 s** antes de un reinicio y **10 s** antes de una conexión nueva.
5. Cuando el `spare` abre, se convierte en el enlace principal, se retira el
   anterior y se cierra. Los fingerprints retirados se recuerdan (32) para no
   aceptar ofertas viejas.
6. Si pasan **60 s** sin ruta, el host elimina al amigo; el join falla con un
   mensaje claro.
7. Si el join no encuentra al host en **90 s**, falla y ofrece *Try again*.

El host no inicia reconexiones: espera a que el join vuelva a ofertar. El join
es quien tiene la iniciativa porque conoce el reloj de su propia caída.

## 9. Arranque del juego

El motor no lanza el juego; lo hace `app.LaunchSession`:

Host:

```text
--server 7777 --playercount {2|4|8|16} --skip-intro
```

Join:

```text
--client 127.0.0.1 <puerto del puente> --skip-intro
```

Antes de lanzar, el host escribe en `sm64config.txt` (preservando el resto del
archivo y sus CRLF):

```text
coop_host_save_slot 3
coop_host_port 7777
```

El host marca `gameIn` en cuanto lanza el juego. El join lo marca cuando el
puente ve el primer datagrama del juego (también si el juego se abrió a mano).

Cuando el juego se cierra, la sala **no** se cierra:

- `gameIn` vuelve a `false`;
- el join olvida `gameAddr`, porque un juego reiniciado puede usar otro puerto
  local; el siguiente datagrama lo fija de nuevo;
- el enlace WebRTC y el código siguen vivos, así que se puede volver a abrir el
  juego desde **Jugar con amigos** sin reiniciar la partida.

Esto reproduce `tC()`/`zS()` del cliente de escritorio.

### Los mods y la partida viajan solos

El loader no copia los mods al unirse, y no hace falta que lo haga: **el propio
juego** pide la lista de mods activos del anfitrión, compara el MD5 de cada
archivo con su caché y descarga los que le faltan en `.tmp/<sesión>/` antes de
entrar. La partida de 512 bytes también viaja en el `PACKET_JOIN`, y las
escrituras del amigo se mandan al anfitrión.

El puente solo tiene que dejar pasar esos datagramas sin tocarlos. Como son
paquetes **fiables** de la capa del juego, el DataChannel sin reintentos no
rompe la descarga: el juego reintenta por su cuenta.

Los detalles (paquetes, tamaños, caché, límites y seguridad) están en
[MOD_SYNC.md](MOD_SYNC.md). Lo importante para la UI: el amigo **no** necesita
instalar los mods del anfitrión antes de unirse.

## 10. Estados que ve la interfaz

```json
{
  "mode": "host",
  "code": "K7QD-9XPA",
  "phase": "connected",
  "friends": [
    { "id": "LQSPQZ…", "route": "direct", "reconnecting": false, "game": true }
  ],
  "trackers": 5,
  "message": null,
  "gameIn": true,
  "port": 7777
}
```

- `trackers`: cuántos puntos de encuentro están abiertos.
- `route`: `direct`, `relay` o `null` mientras se mide.
- `reconnecting`: el enlace se cayó y se está reteniendo el lugar.
- `game`: si el amigo ya abrió su juego (null hasta el primer ping).
- `port`: solo en join; es el puerto que se le pasa al juego.

## 11. Seguridad y límites

- **40 bits y clave derivada del código**: un atacante que vea el `infoHash`
  puede intentar fuerza bruta offline del código (2^40 × 600 000 iteraciones).
  Es caro, pero no son 128 bits. No reutilices códigos y no los publiques.
- **El SDP va cifrado**, pero el tráfico del juego va dentro de DTLS-SRTP, no
  con una capa extra nuestra. La confidencialidad depende de WebRTC.
- **Se comparte la IP** con los trackers públicos, el relay y los STUN de
  Google/Cloudflare. La UI pide consentimiento antes de empezar.
- **Los trackers son de terceros.** Para producción, apunta
  `COOP_RELAY_URL` a tu propio Worker y, si quieres, desactiva los públicos con
  `COOP_NO_TRACKERS=1`.
- **No hay moderación de sala**: quien tenga el código entra. No hay lista de
  baneados, ni contraseña adicional, ni límite de intentos por IP en el
  cliente.
- **No se valida el contenido del juego**: los datagramas se reenvían tal cual,
  con el límite de 4096 B.
- **El anfitrión ejecuta Lua en tu juego.** Al unirse, el juego descarga los
  mods activos del anfitrión y ejecuta su Lua con los permisos del proceso. El
  MD5 detecta corrupción, no autentica al emisor. Únete solo a gente de
  confianza. Ver [MOD_SYNC.md](MOD_SYNC.md).

Mejoras recomendadas para una versión 3 propia (documentadas en
`PROTOCOL.md`): 128 bits de clave, PAKE (SPAKE2/OPAQUE) y sala versionada
`room/v3`.

## 12. Diferencias con el cliente de referencia

| Pieza | Referencia | Este port |
|---|---|---|
| Señalización | WebView/Electron aparte | goroutines en el proceso Go |
| Ventana auxiliar | se reabre si el renderer muere | no hace falta |
| Puerto join | efímero en escritorio, 7777 en Android | efímero |
| `gameIn` host | al lanzar | al lanzar |
| `gameIn` join | primer paquete del juego | primer paquete del juego |
| Detección de 7777 ocupado | sondeo cada 2 s en Android | no aplica en escritorio |
| Servicio en primer plano | Android | no aplica en escritorio |
| Sincronización de mods | la hace el juego (`.tmp` + `mod.cache`) | igual; el puente es transparente |
| Copia entre dispositivos | `copy/v1` | ✅ (ver [COPY.md](COPY.md)) |

## 13. Cómo probarlo

Pruebas automáticas:

```bash
go test ./internal/session -v
```

Cubren:

- generación/normalización y vector PBKDF2;
- cifrado/descifrado y manipulación del Sello;
- `info_hash` como string binario;
- puente host y join con sockets UDP falsos;
- `STILL_HERE` exacto y caché de 2 MiB con reenvío ordenado;
- sesión completa host↔join con un relay WebSocket local;
- corte del enlace, `STILL_HERE` en el juego del host y reconexión con una
  conexión nueva;
- estado, rutas, puerto y apagado ordenado.

Ejecución real:

```bash
go build -trimpath -o coop-loader ./cmd/coop-loader
./coop-loader
```

**Jugar con amigos** (botón de la barra superior):

1. **Host a game** → aparece un código; se copia solo.
2. El amigo abre **Join a game**, escribe el código y pulsa **Join**.
3. Cuando la sesión dice *Connected*, cualquiera pulsa **Open the game**.
4. En el juego: **Host → Direct Connection → Host** o
   **Join → Direct Connection → Join** (la dirección debe ser `localhost` o
   `127.0.0.1`). El aviso de *port forwarding* se ignora.

Variables de entorno:

| Variable | Efecto |
|---|---|
| `COOP_RELAY_URL` | usa otro relay (Worker) en vez del de referencia |
| `COOP_NO_TRACKERS=1` | no abre los cuatro trackers públicos |

El registro de conexión de la UI muestra IDs cortos, ruta, heartbeats y
caídas. No escribe el código ni los SDP; los puntos de encuentro aparecen por
su nombre de host.
