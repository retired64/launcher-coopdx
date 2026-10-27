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
| Cero dependencias | compila en cualquier SO sin proxy, sin cgo, sin supply chain |
| `embed` para la UI | el binario es todo el programa; no hay rutas relativas |
| Token por ejecución | cualquier web podría hacer POST a `localhost` sin él |
| Respaldo + atómico + papelera | los datos del jugador son irrecuperables si se pierden |
| Validar `main.lua` y checksum | el juego no valida lo que el loader escribe |
| Sin borrar mods | un clic equivocado no debe costar una descarga de 300 MB |
