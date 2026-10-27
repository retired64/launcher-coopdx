# El aspecto del juego (fase 5)

La interfaz deja de tener un tema propio y usa **las imágenes y las letras de la
ROM del jugador**, igual que el cliente de referencia. No se incluye ningún
recurso de Nintendo en el repositorio: todo se lee en tiempo de ejecución del
archivo que el jugador ya tiene para `sm64coopdx`.

## 1. Qué se lee de la ROM

El loader solo acepta la ROM que usa el juego:

```text
Super Mario 64 (USA), revisión 0
SHA-1  9bef1128717f958171a4afac3ed78ee2bb4e86ce
```

Acepta el archivo en cualquiera de los tres órdenes de bytes habituales
(`.z64`, `.v64`, `.n64`) y lo normaliza antes de comprobarlo.

### Bloques MIO0

Tres bloques del ROM contienen lo que usa la interfaz:

| Bloque | Offset | Contenido |
|---|---:|---|
| Texturas y fuente de menú | `0x2A65B0` | mármol, maderas, cara de Mario, manos, caja `?`, fuente de menú |
| Cajas de gorras | `0x1F2200` | cajas de ala, metal, invisible, contorno y `?` |
| HUD, diálogo y nombres | `0x108A40` | letras del HUD, fuente de diálogo y nombres de niveles |

El bloque se descomprime con el algoritmo **MIO0** de Nintendo 64: una palabra
de bits indica si toca un byte literal o una referencia hacia atrás
(`longitud = (palabra >> 12) + 3`, `distancia = (palabra & 0xFFF) + 1`).

### Texturas

Se extraen 16 texturas de 32×32 o 64×32 en formato `rgba16`:

```text
marble_light  marble_dark  mario_head  mario_head_gray
wood_red      wood_blue    wood_yellow wood_green      wood_pink
hand_open     hand_point
box_wing      box_metal    box_vanish  box_outline     box_point
```

La decodificación `rgba16` usa el mismo formato que el juego: 5 bits por canal,
1 bit de alfa, y expansión `(v << 3) | (v >> 2)`. Las manos se guardan también
al doble de tamaño (`@2x`) con escalado de vecino más cercano.

### Letras del HUD

El HUD del juego tiene 38 dibujos de 16×16: dígitos, letras, puntuación y los
iconos `coin`, `mario` y `star`. Se guardan como PNG y se describen en
`hud.json`.

La ROM original no trae **J, Q, V, X, Z** ni algunos signos. `sm64coopdx` los
añade a su ejecutable. El loader los busca también ahí:

- lee el ELF/PE de 64 bits del juego;
- busca los símbolos `texture_hud_char_*`;
- comprueba la entrada de la tabla de recursos (`0x1000000010`, 512 bytes);
- copia los 512 bytes del bloque `rgba16`.

Si el ejecutable no se puede leer (por ejemplo, un Mach-O de macOS), esas pocas
letras caen a la fuente de menú; el resto del tema sigue funcionando.

### Fuentes

La ROM guarda las dos fuentes como mapas de bits, no como fuentes vectoriales.
El loader las convierte a TrueType:

| Fuente | Celda | Origen | Uso |
|---|---:|---|---|
| `SM64 Dialog` | 16 px | `ia4`, 16×8 rotado | texto, botones, nombres |
| `SM64 Menu` | 8 px | `ia8`, 8×8 | títulos de grupo y etiquetas |

Cada píxel se convierte en un rectángulo de la fuente. Se conservan:

- el ancho de avance de cada letra;
- la línea base calculada con la `A`;
- las mayúsculas y minúsculas que trae el juego;
- las minúsculas de la fuente de menú como alias de las mayúsculas;
- los acentos `á é í ó ú ü ñ ç` y sus mayúsculas, construidos píxel a píxel
  sobre la letra base;
- `¡` y `¿`, reflejando `!` y `?`.

El resultado se escribe como `dialog.ttf` y `menu.ttf` (y una copia con la
extensión `.otf` por compatibilidad). Son fuentes TrueType con contornos
`glyf`.

### Nombres de niveles

Del mismo bloque del HUD se leen los 26 nombres de niveles y los 90 nombres de
estrella. Se guardan en `names.json` para la ficha de una partida o para el
futuro editor de estrellas. No se muestran como texto traducido: son los
nombres que trae el juego.

## 2. Dónde se guarda

Se extrae a:

```text
<config>/coop-loader/rom-assets/
├── dialog.ttf
├── menu.ttf
├── dialog.otf   (la misma fuente, nombre histórico)
├── menu.otf
├── hud.json
├── names.json
├── version.json
├── hud/
│   ├── u0041.png … u005a.png
│   ├── coin.png
│   ├── mario.png
│   └── star.png
└── textures/
    ├── wood_yellow.png …
    ├── marble_dark.png
    ├── mario_head.png
    └── hand_open@2x.png …
```

`version.json` guarda la versión del formato, el SHA-1 de la ROM y una huella
del ejecutable del juego. Si cambia cualquiera de los tres, se vuelve a
extraer. Si no hay ROM pero la caché está completa, la interfaz sigue usándola.

Borrar esa carpeta es seguro: se reconstruye la próxima vez que se abra el
loader.

## 3. Cómo se busca la ROM

En este orden:

1. la ruta guardada en `settings.json` (`romPath`);
2. `<datos del juego>/baserom.us.z64`;
3. `<carpeta del juego>/baserom.us.z64`.

En **Opciones → Estilo del juego → Usar otro archivo** se puede elegir cualquier
`.z64`, `.n64` o `.v64`. El navegador lo envía al servidor local, se valida y se
extrae; el archivo no se guarda en el repositorio ni se manda a internet.

## 4. Cómo se usa en la interfaz

### Fondo de cada pestaña

La textura de madera se escala a 640×640 y se repite. Cada pestaña elige una:

| Pestaña | Madera |
|---|---|
| Mis mods, Partidas, Perfiles | `wood_yellow` |
| Buscar | `wood_blue` |
| Jugador | `wood_red` |
| Crear | `wood_pink` |
| Opciones, Copiar | `wood_green` |
| Jugar con amigos | `wood_blue` |

La barra lateral usa **la misma madera que la pestaña activa**, así el menú
cambia de color junto con la pantalla (azul en Buscar y Jugar, rojo en Jugador,
rosa en Crear, verde en Opciones y Copiar, amarillo en Mis mods y Partidas). El
cambio se hace con `data-view` en `<html>` y la clase `rom`.

### Títulos y logotipo

`<sm64-text>` es un elemento propio: convierte cada letra en una imagen del HUD
y las solapa igual que el juego. Se usa en:

- `COOP MODS` (logotipo);
- `MIS MODS`, `BUSCAR`, `JUGADOR`, `PARTIDAS`, `CREAR`, `OPCIONES`;
- `JUGAR CON AMIGOS`, `COPIAR ENTRE DISPOSITIVOS`;
- `NUEVO` en las ranuras vacías;
- los contadores de estrellas (`×16`).

Si no hay ROM, `<sm64-text>` cae a texto normal con la fuente de menú.

### Botones, cajas y caras

- Botón normal: mármol oscuro.
- Botón principal / Jugar: madera verde.
- Instalar / enlazar: madera azul.
- Peligro / borrar: madera roja.
- Ranuras de partida: cara de Mario (`mario_head`) sobre mármol; las vacías usan
  la cara gris.
- Interruptor de mods: cara de Mario en verde cuando está activo, cara gris
  cuando no.
- Estrellas y monedas: iconos del HUD.
- Cursor: mano abierta y mano cerrada de la ROM.

Todo usa `image-rendering: pixelated` y sombras duras de 2 px para conservar el
aspecto de pixel art.

### Jugador

La pestaña **Jugador** lee y escribe el aspecto que el juego guarda en
`sm64config.txt`:

- `coop_player_name`;
- `coop_player_model` (Mario, Luigi, Toad, Waluigi, Wario);
- `coop_player_palette_<parte>` para las ocho partes.

Los presets son los `.ini` de `<carpeta del juego>/palettes` (integrados) y de
`<datos del juego>/palettes` (del jugador). Guardar un preset escribe el `.ini`
y recuerda el personaje en `player-presets.json`. Borrar un preset lo mueve a
`removed-by-coop-loader/presets-<fecha>/`; nunca se borra.

### Acordeón y movimiento

La pantalla de **Opciones** es un acordeón: cada sección es una cabecera con la
misma barra de grupo que usan los mods (título, pista y flecha). Al pulsar una
cabecera se cierra la anterior y se abre la nueva con una transición de altura;
solo queda una abierta. Los grupos de **Mis mods** también se pueden plegar,
pero recuerdan su estado y pueden estar todos abiertos a la vez.

El movimiento copia el del cliente de referencia, sin dependencias:

- **Cambio de pestaña**: se clona la pantalla anterior en una capa fija, se
  recorta con `clip-path` en la dirección del movimiento y se retira en 190 ms;
  la nueva dibuja sus bloques con un barrido y un pequeño escalonado.
- **Título**: cada letra del HUD sube y se va antes de que el título nuevo
  entre letra a letra.
- **Barra lateral**: el marcador se desliza con un muelle entre secciones.
- **Botones**: se levantan 1 px al pasar el ratón, se hunden al pulsar y el
  botón **Jugar** da un pequeño empujón.
- **Filas y tarjetas**: se desplazan 2 px y cambian el fondo.
- **Interruptores**: la cara de Mario crece y el interruptor hace un rebote.
- **Diálogos y avisos**: entran con caída y rebote; el foco dibuja su contorno.
- **Cursor**: mano abierta; al pulsar, mano cerrada.

Todo se desactiva con `prefers-reduced-motion: reduce`, y las animaciones usan
solo CSS y la API `Element.animate` del navegador.

## 5. API

```text
GET  /api/rom/status          {rom: {ready, reason, message, path, extraHud, busy}}
POST /api/rom/path            {path}
POST /api/rom/upload?name=…   cuerpo binario de la ROM
GET  /rom-assets/<archivo>    PNG, OTF y JSON de la caché
```

`/api/rom/status` arranca la extracción en segundo plano la primera vez y
devuelve `extracting`; la UI vuelve a preguntar hasta que termina.

## 6. Seguridad y privacidad

- La ROM se lee en local. No se sube a ningún servidor.
- El SHA-1 se comprueba antes de usar ningún offset.
- Las rutas de `/rom-assets/` se validan y no se puede salir de la caché.
- El servidor sigue escuchando solo en loopback y exigiendo `X-Token` en
  `/api/*`.
- La interfaz no incluye anuncios, cuentas ni telemetría.

## 7. Diferencias y límites

- **No se incluye la vista 3D del personaje.** El cliente original dibuja un
  Mario 3D leyendo el ejecutable del juego. Eso es la fase 6; esta fase cubre
  texturas, letras, fondos y el editor de aspecto.
- La ROM debe ser la USA 1.0 que usa `sm64coopdx`. Otra región o revisión no
  vale, aunque sea Super Mario 64.
- Las letras extra del HUD se leen de ejecutables ELF/PE de 64 bits. En macOS
  (Mach-O) o en un ejecutable desconocido, J/Q/V/X/Z y algunos signos usan la
  fuente de menú como respaldo.
- La caché se puede borrar; no contiene nada que no se pueda reconstruir de la
  ROM.
- Las capturas de la documentación muestran el tema con recursos extraídos de
  una ROM propiedad del autor. Los archivos extraídos (PNG y OTF) **no** están
  en el repositorio.

## 8. Cómo probarlo

Automático:

```bash
go test ./internal/rom ./internal/app ./internal/server
```

La prueba de extracción contra una ROM real es opcional:

```bash
COOP_TEST_ROM="$HOME/.local/share/sm64coopdx/baserom.us.z64" \
COOP_TEST_ASSETS="$HOME/.config/sm64-modloader/rom-assets" \
COOP_TEST_EXE="$HOME/sm64coopdx_Linux-1.5.1-autoUpdater/sm64coopdx" \
go test ./internal/rom -run TestExtractAgainstReference -v
```

Compara píxel a píxel las 16 texturas, los 50 dibujos del HUD (incluidas las
letras extra), `names.json` y las métricas de las dos fuentes.

Manual:

1. Abre el loader.
2. **Opciones → Estilo del juego → Usar otro archivo** y elige tu ROM.
3. Espera a «Estilo del juego cargado».
4. Recorre **Mis mods**, **Buscar**, **Jugador**, **Partidas**, **Crear** y
   **Opciones**: cada uno debe tener su madera, sus títulos del HUD y sus
   botones de mármol.
