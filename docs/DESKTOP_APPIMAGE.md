# Escritorio nativo y AppImage

> **Estado: propuesta.** Todavía no está implementado. Hoy el loader abre la
> interfaz en el navegador; este documento es el plan para que use una ventana
> nativa y se pueda distribuir como AppImage y, más adelante, como `.deb`.

## 1. Objetivo

Que el usuario abra Coop Mod Loader como un programa normal:

- doble clic en el binario o en la AppImage;
- ventana propia con icono, título y tamaño recordado;
- sin pestaña del navegador, sin barra de direcciones y sin URL visible;
- el mismo aspecto, la misma interfaz y las mismas funciones actuales;
- un solo ejecutable de escritorio con los archivos web dentro.

La UI no se reescribe. Se reutiliza el HTML, CSS y JavaScript que ya están
embebidos en el binario.

## 2. Decisión recomendada

Usar **Wails v3** como capa de ventana nativa:

| Sistema | Motor que usa |
|---|---|
| Linux | WebKitGTK 4.1 |
| Windows | WebView2 |
| macOS | WKWebView |

Wails v3 se elige por encima de otras opciones porque:

- reutiliza la interfaz web actual sin cambios grandes;
- da ventana, icono, diálogos nativos, enlaces externos y ciclo de vida;
- permite empaquetar la app para Linux sin añadir Electron ni Chromium;
- en Windows y macOS usa el WebView que ya trae el sistema.

Si en el momento de implementarlo Wails v3 no está estable para el toolchain
disponible, la alternativa es Wails v2 con el soporte de WebKitGTK 4.1. No se
recomienda reescribir la UI en Fyne/Gio ni empaquetar Chromium.

### Alternativas descartadas

- **Fyne o Gio puros:** ventana sin cgo, pero obliga a rehacer toda la interfaz.
- **webview_go:** válido y más pequeño, pero ofrece menos herramientas de
  empaquetado, diálogos y ciclo de vida que Wails.
- **Chromium en modo aplicación:** sigue siendo un navegador empaquetado y pesa
  mucho.
- **Electron:** repetiría el cliente original y perdería la ventaja de Go.

## 3. Cómo se conecta la ventana con el backend

La arquitectura actual ya separa bien las responsabilidades:

- `internal/app` contiene la lógica.
- `internal/server` contiene las rutas HTTP y los archivos web embebidos.
- `internal/session`, `internal/devicecopy`, `internal/rom`, etc. no saben nada
  del navegador.

El plan es:

1. Extraer el arranque y la parada del servidor a un paquete reutilizable.
2. El modo escritorio arranca el servidor interno en `127.0.0.1`, en un puerto
   efímero y con el token aleatorio que ya existe.
3. Wails abre una ventana apuntando a esa dirección interna.
4. El usuario nunca ve la URL, el puerto ni el token.
5. Al cerrar la ventana se detiene el servidor y se cierra todo limpiamente.

Ese `localhost` es solo comunicación interna entre la ventana y el backend Go.
No es un navegador ni requiere que el usuario abra nada.

### Alternativa más estricta

Se podría evitar también el puerto local usando un esquema interno de Wails y
puentes nativos. Eso obliga a cambiar las llamadas `fetch` de la interfaz por
bindings de Wails. Es más trabajo y más riesgo para poca ganancia visible.

**Decisión para la primera versión:** puerto loopback efímero, invisible para
el usuario. Si más adelante se quiere eliminar del todo, se estudia el esquema
interno.

## 4. Qué se conserva y qué se adapta

### Se conserva

- mods;
- partidas;
- perfiles;
- catálogo;
- sesión P2P;
- copia entre dispositivos;
- extracción de la ROM;
- tema visual y animaciones;
- API HTTP y pruebas actuales.

### Se adapta

- abrir enlaces externos con el navegador del sistema;
- elegir archivos con diálogos nativos si WebKit no se comporta igual;
- comprobar `<input type="file">` dentro de WebKitGTK;
- asegurar que «cerrar el loader al jugar» cierre la ventana sin matar el
  juego;
- evitar dos instancias del programa;
- recordar tamaño y posición de la ventana;
- mostrar un error claro si WebKitGTK no está instalado.

## 5. Estructura propuesta

```text
cmd/
  coop-loader/            modo CLI/servidor, sin cgo
  coop-loader-desktop/    entrada de escritorio con Wails
internal/
  desktop/                ventana, ciclo de vida, icono, diálogos nativos
  host/                   arranque y parada del servidor interno
  server/                 se queda como está
  app/                    se queda como está
packaging/
  appimage/               AppRun, .desktop, icono y empaquetado
  deb/                    control, dependencias y postinst
```

El núcleo sigue siendo ligero y probable con `go test`. Solo el ejecutable de
escritorio usa cgo y WebKit.

### Por qué dos ejecutables

- `coop-loader` conserva el modo servidor/navegador para desarrollo, pruebas y
  usuarios que lo prefieran.
- `coop-loader-desktop` es el que se empaqueta como AppImage y `.deb`.
- Los paquetes internos (`internal/app`, `internal/server`, etc.) no cambian.
- Las pruebas de lógica y HTTP siguen corriendo sin entorno gráfico.

## 6. Fases

### Fase 1: prototipo de ventana

- Instalar en Fedora `webkit2gtk4.1-devel`, `gtk3-devel` y las dependencias de
  Wails.
- Crear la ventana nativa.
- Cargar la UI actual.
- Verificar que todas las llamadas a la API funcionan.
- Probar ROM, mods, partidas, jugador, sesión y copia.
- Cerrar ventana y servidor correctamente.
- Comprobar que el proceso del juego sobrevive al cierre de la ventana cuando
  se elige «cerrar el loader al jugar».

### Fase 2: integración fina

- Diálogos nativos para carpeta del juego y archivo ROM si hacen falta.
- Enlaces externos con el navegador del sistema.
- Tamaño y posición de ventana.
- Icono y título.
- Instancia única.
- Manejo de errores si WebKit no está disponible.
- Revisar que arrastrar/soltar archivos y los selectores de archivo funcionan
  dentro del WebView.

### Fase 3: AppImage

- Compilar el binario de escritorio.
- Incluir el `.desktop`, el icono y el AppRun.
- Generar la AppImage con `appimagetool`.
- Probar en Fedora y en otra distribución limpia.
- Definir si WebKitGTK se exige del sistema o se empaqueta.
- Comprobar que la AppImage puede leer la carpeta del juego y la ROM fuera de
  la AppImage.

### Fase 4: `.deb`

- Dependencias: `libwebkit2gtk-4.1-0` y `libgtk-3-0`.
- Instalar binario, icono y entrada de menú.
- Probar instalación, actualización y desinstalación.
- Revisar que no deja archivos del usuario al desinstalar.

### Fase 5: pulido y CI

- Prueba automática del handler HTTP sin ventana.
- Prueba de arranque de la ventana bajo Xvfb.
- Comprobar que no se abre ninguna pestaña del navegador.
- Documentar dependencias por distribución.
- Preparar el `Makefile` o script de empaquetado.

## 7. El punto delicado: AppImage y WebKit

Una AppImage puede contener:

- el binario Go;
- los archivos web embebidos;
- el icono;
- el `.desktop`;
- el AppRun.

Pero **WebKitGTK normalmente se usa del sistema**. No conviene empaquetar
WebKit entero en la primera versión: pesa mucho y complica la AppImage.

Consecuencia:

- la AppImage funcionará en sistemas que tengan WebKitGTK 4.1;
- en Fedora se resuelve con el paquete del sistema;
- en el `.deb` se declara como dependencia;
- si más adelante se quiere una AppImage totalmente autónoma, habría que
  estudiar empaquetar WebKit o cambiar de enfoque, con un coste grande de
  tamaño y mantenimiento.

## 8. Riesgos y mitigaciones

| Riesgo | Mitigación |
|---|---|
| Wails exige cgo y toolchain de C | Mantener el núcleo puro y usar cgo solo en `cmd/coop-loader-desktop`. |
| WebKitGTK no está en todas las distribuciones | Documentar dependencia y declararla en `.deb`; probar AppImage en sistemas limpios. |
| Diferencias de WebKit con Chromium | Probar file inputs, enlaces externos, portapapeles y descargas. |
| El servidor interno queda abierto | Loopback, puerto efímero, token aleatorio y cierre al cerrar ventana. |
| El juego se cierra al cerrar el loader | Lanzar el juego como proceso independiente y probar ese flujo. |
| Dos instancias abren dos puertos | Bloqueo de instancia única. |
| La AppImage no arranca por glibc | Compilar en una distribución base antigua y probar en varias. |
| La ventana tarda en mostrar la UI | Ventana con pantalla de carga mientras el servidor arranca; la UI actual ya consulta el estado de la ROM. |

## 9. Criterios de aceptación

La fase de escritorio estará terminada cuando:

- el binario abra una ventana nativa sin navegador;
- no se muestre ninguna URL al usuario;
- todas las pestañas y flujos actuales funcionen igual;
- la ROM y los archivos del juego se lean desde fuera del binario;
- cerrar la ventana detenga el servidor;
- «cerrar el loader al jugar» no cierre el juego;
- la AppImage arranque en Fedora con WebKitGTK instalado;
- el `.deb` declare sus dependencias y funcione tras instalar;
- `go test ./...` y las pruebas de la API sigan pasando.

## 10. Estimación

- **Fase 1:** 1–2 días.
- **Fase 2:** 1 día.
- **Fase 3 (AppImage):** 1–2 días.
- **Fase 4 (`.deb`):** 1 día.
- **Fase 5 (pulido y CI):** 1 día.

Total aproximado: **5–7 días** para una primera versión de escritorio con
AppImage y `.deb`, sin contar imprevistos de WebKit o de empaquetado.

## 11. Orden recomendado

1. No tocar la UI.
2. Crear `cmd/coop-loader-desktop` con Wails v3.
3. Mantener el servidor interno en loopback con puerto efímero y token.
4. Conservar `coop-loader serve` para desarrollo, pruebas y modo navegador.
5. Empaquetar primero una AppImage que use WebKitGTK del sistema.
6. Generar después el `.deb`.
7. Dejar para el final la AppImage totalmente autónoma con WebKit embebido.

El resultado esperado: el usuario hace doble clic en la AppImage, se abre una
ventana de Coop Mod Loader con el mismo aspecto actual, sin navegador, sin URL
visible y con el juego funcionando igual.
