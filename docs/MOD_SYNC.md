# Sincronización de mods al unirse (la hace el juego)

Este documento corrige una idea equivocada que estaba en el README: **los dos
jugadores no necesitan tener instalados los mismos mods para jugar**. El juego
del anfitrión envía sus mods activos al juego que se une, y este los guarda en
una carpeta temporal propia antes de entrar a la partida.

El loader no participa en esa transferencia. Su puente UDP es transparente: los
paquetes del juego (incluidos el catálogo de mods y los trozos de archivo) pasan
por el DataChannel como cualquier otro datagrama.

Fuente analizada: `~/sm64coopdx`, versión **v1.5.1**, commit
`8cd6e5977d9f920d51ca71f2c61801d019ed79c6`. Las rutas y números de línea de
abajo son de ese árbol.

## 1. Resumen

```text
Juego del anfitrión                         Juego del amigo
──────────────────                         ───────────────
gActiveMods (solo mods activos)
        │
        │  PACKET_MOD_LIST_REQUEST  ◄────────  al conectar
        │  PACKET_MOD_LIST + ENTRY + FILE  ──►
        │                                      compara MD5 con mod.cache
        │  PACKET_DOWNLOAD_REQUEST  ◄────────  pide grupos de 40 KB
        │  PACKET_DOWNLOAD (800 B)  ─────────►  escribe en .tmp/<sesión>/
        │                                      activa los mods remotos
        │  PACKET_JOIN_REQUEST      ◄────────  solo cuando terminó
        │  PACKET_JOIN (incluye EEPROM) ─────►  entra a la partida
```

Consecuencias:

- El amigo **no** tiene que instalar nada antes. Al unirse, su juego descarga lo
  que le falte.
- Solo se envían los mods **activados en el anfitrión**. Un mod instalado pero
  desactivado no viaja y tampoco se activa en la partida.
- Los mods del amigo que no estén en el anfitrión **no se usan** durante esa
  sesión: el juego activa el conjunto remoto, no el local.
- Si el amigo ya tiene el mismo archivo (mismo MD5), no se descarga: se reutiliza
  su copia local.
- La descarga es de **archivos crudos**, no del `.zip` ni del sitio de mods.
- El juego también recibe la **partida del anfitrión** (512 bytes de EEPROM) y
  las escrituras del cliente se mandan al anfitrión. La fase 4 no es necesaria
  para eso tampoco.

## 2. Cuándo empieza

El cliente pide la lista en cuanto inicializa el socket, antes de que el
anfitrión le mande el `PACKET_JOIN`:

- `src/pc/network/socket/socket.c:190` → `network_send_mod_list_request()`.
- Lo mismo para CoopNet en `src/pc/network/coopnet/coopnet.c:88`.
- `network_send_mod_list_request` está en
  `src/pc/network/packets/packet_mod_list.c:10`.

El cliente limpia `gActiveMods` y `gRemoteMods`, genera la carpeta temporal y
manda la petición con su versión del juego.

La carpeta temporal se genera en `mods_generate_remote_base_path`
(`src/pc/mods/mods.c:103`):

```text
<directorio de escritura del juego>/.tmp/<6 dígitos hex aleatorios>/
```

Es un nombre de sesión, no el nombre del mod. Ejemplo:
`~/.local/share/sm64coopdx/.tmp/3F2A91/`.

## 3. Qué manda el anfitrión

`network_receive_mod_list_request` (`packet_mod_list.c:31`) responde con
`network_send_mod_list` (`packet_mod_list.c:41`). Todo va en paquetes
**fiables** y dentro de un grupo **ordenado** (`packet_ordered_begin/end`).

Primero, un paquete de cabecera:

| Campo | Tipo | Notas |
|---|---|---|
| versión | 128 bytes | `get_version()`, comparación exacta |
| cantidad de mods | `u16` | |

Después, un `PACKET_MOD_LIST_ENTRY` por mod:

| Campo | Tipo | Notas |
|---|---|---|
| índice | `u16` | |
| largo del nombre | `u16` | |
| nombre | bytes | máximo 64 |
| largo de incompatibilidades | `u16` | |
| incompatibilidades | bytes | máximo 256, puede ir vacío |
| largo de la ruta relativa | `u16` | |
| ruta relativa | bytes | nombre de carpeta o archivo `.lua` |
| tamaño | `u64` | suma de sus archivos |
| es carpeta | `u8` | |
| pausable | `u8` | |
| ignorar avisos de script | `u8` | |
| cantidad de archivos | `u16` | |

Y un `PACKET_MOD_LIST_FILE` por archivo:

| Campo | Tipo | Notas |
|---|---|---|
| índice del mod | `u16` | |
| índice del archivo | `u16` | |
| largo de la ruta relativa | `u16` | |
| ruta relativa | bytes | dentro del mod |
| tamaño | `u64` | |
| hash | 16 bytes | **MD5** del contenido |

Al final, `PACKET_MOD_LIST_DONE`.

El anfitrión recorre `gActiveMods`, que se llenó en `network_init` con
`mods_activate(&gLocalMods)` (`src/pc/network/network.c:167`). Por eso solo
viajan los mods activos.

Antes de enviar, `mod_activate` actualiza el MD5 de cada archivo en el
anfitrión (`src/pc/mods/mod.c:152-162`). Así el hash que recibe el cliente es
el del archivo que realmente se va a leer.

### Qué archivos entran en un mod

`mod_load_files` (`src/pc/mods/mod.c:350`) no manda la carpeta entera. Incluye:

- `.lua` y `.luac`, recursivo en todo el mod;
- `actors/*.bin` y `actors/*.col` (sin recursión);
- `data/*.bhv`;
- `textures/*.tex`;
- `levels/*.lvl`;
- `sound/*.m64`, `.mp3`, `.aiff`, `.ogg`.

Los JSON, README, imágenes fuente y demás archivos no forman parte del mod
cargable por el juego, así que tampoco se sincronizan.

Un mod de un solo archivo (`.lua` suelto) tiene `isDirectory = false` y su
`relativePath` es el nombre del archivo. Un mod de carpeta tiene
`isDirectory = true` y `relativePath` es el nombre de la carpeta; sus archivos
llevan rutas relativas dentro de ella.

### Límite de tamaño

`MAX_MOD_SIZE` son **35 MiB** (`src/pc/mods/mods.h:9`). Un mod que llegue a ese
tamaño se desactiva y no se puede seleccionar (`mods_size_enforce` en
`src/pc/mods/mods_utils.c:15`), así que el anfitrión no lo envía.

Si un cliente recibe una entrada con `size >= MAX_MOD_SIZE`, corta la conexión
con el aviso `DISCONNECT_BIG_MOD`
(`src/pc/network/packets/packet_mod_list.c:246`).

## 4. Qué hace el cliente al recibir la lista

`network_receive_mod_list` (`packet_mod_list.c:118`):

1. Comprueba que la **versión del juego** sea idéntica. Si no, cierra y muestra
   «Version mismatch».
2. Lee la cantidad de mods y reserva `gRemoteMods`.
3. Por cada entrada (`network_receive_mod_list_entry`, línea 164):
   - construye `basePath` dentro de `.tmp/<sesión>/`;
   - rechaza mods de 35 MiB o más;
   - reserva la lista de archivos.
4. Por cada archivo (`network_receive_mod_list_file`, línea 261):
   - lee ruta, tamaño y MD5;
   - busca ese MD5 en `mod.cache` con `mod_cache_get_from_hash`
     (`src/pc/mods/mod_cache.c:121`);
   - si lo encuentra y el archivo local sigue teniendo ese MD5, reutiliza la
     ruta local y **no lo descarga**.

`mod.cache` está en el directorio de escritura del juego. Su versión actual es
**7** (`src/pc/mods/mod_cache.c:15`). Guarda por entrada: MD5 de 16 bytes,
`u64 lastLoaded`, largo de ruta y ruta. Si la versión no coincide, el juego
borra `.tmp` entero (`mods_delete_tmp`, línea 281).

Esto explica dos cosas:

- un mod ya descargado en una partida anterior no se vuelve a bajar;
- el mismo archivo dentro de otro mod también se reutiliza, porque la clave es
  el contenido, no el nombre.

## 5. Protocolo de descarga

`network_start_download_requests` (`packet_download.c:36`) calcula el total y
divide el flujo en grupos:

| Constante | Valor |
|---|---:|
| `CHUNK_SIZE` | 800 bytes |
| `OFFSET_COUNT` | 50 |
| `GROUP_SIZE` | 40 000 bytes (800 × 50) |

Los archivos se concatenan en un solo flujo lógico. El cliente pide un offset y
el anfitrión responde con hasta 50 paquetes `PACKET_DOWNLOAD`, cada uno con:

```text
u64 offset
u64 longitud        (≤ 800)
u8  datos[longitud]
```

El cliente mantiene **dos grupos en vuelo**. Cuando uno lleva la mitad y el otro
terminó (o no está activo), arranca el siguiente
(`packet_download.c:210`). Así no espera la latencia de un grupo antes de pedir
el otro.

Detalles:

- Los paquetes de descarga son **fiables** (`packet_init(..., true, ...)`), así
  que la capa de red del juego reintenta los que se pierdan. El DataChannel del
  loader puede ser no fiable sin romper la descarga.
- El paquete máximo del juego es `PACKET_LENGTH = 3000`
  (`src/pc/network/packets/packet.h:10`), por debajo del límite de 4096 B del
  puente. Un chunk de 800 B deja margen para cabeceras y compresión.
- Un chunk duplicado se ignora (`og->rx[j]` ya está marcado).
- Un chunk de un grupo inactivo se ignora.
- La longitud se valida contra `CHUNK_SIZE`.
- Cada archivo se abre al recibir su primer byte; se crean los directorios
  necesarios.
- Al terminar, se hace `flush`/`close` de todos los archivos, se marca
  `mod->enabled = true` en **todos** los mods remotos y recién entonces se manda
  `PACKET_JOIN_REQUEST` (`packet_download.c:186`).

Progreso que ve el jugador: el panel «JOINING» con dos barras
(`src/pc/djui/djui_panel_join_message.c:55`) y una estimación de tiempo basada en
los bytes y la velocidad.

### Mods WIP

Si el nombre del mod contiene `(wip)` o `[wip]` (sin distinguir mayúsculas), el
archivo **no** se escribe en `.tmp`: se abre con `f_open_w`, el sistema de
archivos en memoria (`src/pc/fs/fmem.c:75`), y desaparece al cerrar. Es una
decisión del juego para no cachear mods en desarrollo.

## 6. Entrada a la partida

Cuando la descarga termina, el cliente manda `PACKET_JOIN_REQUEST`
(`src/pc/network/packets/packet_join.c:39`). El anfitrión responde con
`PACKET_JOIN` (línea 78), que además de la configuración de la partida incluye
**la EEPROM de 512 bytes del anfitrión**:

```c
fs_file_t* fp = fs_open(SAVE_FILENAME);
if (fp != NULL) {
    fs_read(fp, eeprom, 512);
    fs_close(fp);
}
...
packet_write(&p, eeprom, sizeof(u8) * 512);
```

El cliente hace `gOverrideEeprom = eeprom` y luego `save_file_load_all(TRUE)`
(`packet_join.c:143` y `:193`). Es decir, durante la partida usa la partida del
anfitrión. Cuando el cliente guarda, `save_file_do_save` manda
`PACKET_SAVE_FILE` al anfitrión (`src/game/save_file.c:407`), que es quien
escribe. Al salir, `gOverrideEeprom` vuelve a `NULL` y el cliente sigue con su
propia partida local.

Después de eso:

```c
mods_activate(&gRemoteMods);   // packet_join.c:195
djui_panel_modlist_create(NULL);
smlua_init();
dynos_behavior_hook_all_custom_behaviors();
```

O sea: el juego activa **solo** los mods remotos para esa sesión, muestra la
lista de mods y ejecuta su Lua. Los mods locales del cliente siguen en
`gLocalMods`, pero no forman parte de `gActiveMods` mientras dura la partida.

## 7. Qué no hace

- No modifica `sm64config.txt` para activar los mods remotos. La activación es
  en memoria y dura lo que dura la sesión.
- No borra los mods locales del cliente.
- No descarga desde `mods.sm64coopdx.com`; todo sale del juego del anfitrión.
- No sincroniza mods desactivados en el anfitrión.
- No sincroniza archivos que el juego no carga (JSON, README, imágenes fuente…).
- No borra `.tmp` al desconectarse. Los archivos quedan como caché hasta que
  cambie la versión de `mod.cache`.
- No comprueba que el anfitrión sea de confianza. El MD5 solo detecta
  corrupción/cambios, no autentica al emisor.

## 8. Seguridad: el anfitrión ejecuta código en tu juego

Esto es importante y conviene decirlo en la UI:

- Los mods descargados incluyen **Lua** y el juego lo ejecuta con los permisos
  del proceso. Un anfitrión malicioso puede enviar un mod con código arbitrario.
- Las rutas de archivo (`mod->relativePath` y `file->relativePath`) vienen del
  anfitrión y se concatenan con `concat_path` + `normalize_path`
  (`src/pc/mods/mods_utils.c:209-222`), que **no** eliminan `..`. Un anfitrión
  malicioso podría intentar escribir fuera de `.tmp/<sesión>`.
- El MD5 identifica contenido; no firma ni prueba quién lo creó.

Por eso:

- únete solo a personas que conozcas;
- no publiques códigos de sala;
- si algún día el loader implementa su propia copia (fase 4), debe validar rutas
  (sin `..`, sin absolutas, sin symlinks), usar una carpeta de staging y mover
  con rename atómico; no debe copiar la concatenación ingenua del juego.

## 9. Qué significa para la fase 4

La fase 4 (`copy/v1`) sigue teniendo sentido, pero **no para poder unirse**. El
juego ya resuelve eso. Sirve para:

1. **Preinstalar** mods en el otro dispositivo antes de jugar, incluidos los que
   el anfitrión tiene desactivados o que aún no ha usado.
2. Copiar el **aspecto del jugador**: modelo, colores y presets.
3. Copiar **partidas**: los 512 bytes, nombres de slot y `.sav` de SM64CoopDX.
4. Migrar de PC a Android o viceversa sin depender de una sesión de juego.
5. Conservar la **procedencia** del sitio (`record`), que la sincronización del
   juego no transporta.

La copia del loader no debe intentar hablar el protocolo `PACKET_MOD_LIST` del
juego: es otro canal, con otra sal (`coop-mod-loader/copy/v1`), DataChannel
`copy` ordenado y fiable, manifiesto JSON y SHA-256 por archivo. El contrato
está en [PROTOCOL.md](PROTOCOL.md#8-copia-entre-dispositivos).

Lo que sí conviene reutilizar del juego es su **modelo mental**: solo lo que el
otro lado necesita, archivos crudos, caché por hash, y no tocar el config del
juego para activar cosas ajenas.

## 10. Cómo comprobarlo con el juego real

1. En el anfitrión, instala y activa un mod pequeño (por ejemplo, uno de Lua).
2. En el amigo, asegúrate de **no** tenerlo instalado.
3. Crea la sala y abre los dos juegos con **Direct Connection**.
4. Al unirse, el amigo debe ver el panel **JOINING** con progreso.
5. Al entrar, el mod debe aparecer en la lista de mods (`F1`/menú de mods) y
   funcionar.
6. Comprueba que en el disco del amigo aparece
   `<datos del juego>/.tmp/<6 hex>/...` con los archivos del mod.
7. Cierra la partida, vuelve a unirte: la segunda vez no debe descargar (mismo
   MD5 en `mod.cache`).

No se ha probado todavía con el binario real en este proyecto; la prueba de la
fase 3 usó juegos UDP falsos. Esta lista queda como verificación pendiente.

## 11. Mapa de código

| Función | Archivo:línea |
|---|---|
| petición de lista | `src/pc/network/packets/packet_mod_list.c:10` |
| respuesta del anfitrión | `src/pc/network/packets/packet_mod_list.c:41` |
| recepción de cabecera | `src/pc/network/packets/packet_mod_list.c:118` |
| recepción de mod | `src/pc/network/packets/packet_mod_list.c:164` |
| recepción de archivo | `src/pc/network/packets/packet_mod_list.c:261` |
| fin de lista | `src/pc/network/packets/packet_mod_list.c:316` |
| inicio de descarga | `src/pc/network/packets/packet_download.c:36` |
| petición de grupo | `src/pc/network/packets/packet_download.c:217` |
| envío de chunk | `src/pc/network/packets/packet_download.c:247` |
| recepción de chunk | `src/pc/network/packets/packet_download.c:356` |
| carpeta temporal | `src/pc/mods/mods.c:103` |
| activar conjunto | `src/pc/mods/mods.c:135` |
| archivos que forman un mod | `src/pc/mods/mod.c:350` |
| caché MD5 | `src/pc/mods/mod_cache.c:71` |
| entrada a la partida | `src/pc/network/packets/packet_join.c:137` |
| activar mods remotos | `src/pc/network/packets/packet_join.c:195` |
| panel JOINING | `src/pc/djui/djui_panel_join_message.c:55` |
