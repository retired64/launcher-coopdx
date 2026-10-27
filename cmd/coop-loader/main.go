// Command coop-loader manages SM64 Coop Deluxe mods, saves and mod profiles.
//
// Run it with no arguments to start the local UI, or use a subcommand for
// scripting:
//
//	coop-loader status
//	coop-loader mods list
//	coop-loader mods on character-select-coop
//	coop-loader saves list
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/retired64/coop-loader/internal/app"
	"github.com/retired64/coop-loader/internal/mods"
	"github.com/retired64/coop-loader/internal/server"
)

func main() {
	if len(os.Args) < 2 {
		if err := serve(nil); err != nil {
			fail(err)
		}
		return
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func run(cmd string, args []string) error {
	switch cmd {
	case "serve":
		return serve(args)
	case "status":
		return status()
	case "mods":
		return modsCmd(args)
	case "saves":
		return savesCmd(args)
	case "profiles":
		return profilesCmd(args)
	case "game":
		return gameCmd(args)
	case "version", "-v", "--version":
		fmt.Println(app.Version)
		return nil
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage() {
	fmt.Print(`Coop Loader — mods, saves and profiles for SM64 Coop Deluxe

  coop-loader                     start the local UI
  coop-loader serve [-addr ADDR] [-open=false]
  coop-loader status
  coop-loader mods list
  coop-loader mods on|off ID [--pack]
  coop-loader mods install PATH [--allow-compiled]
  coop-loader mods remove ID [--pack]
  coop-loader saves list
  coop-loader saves copy FROM TO
  coop-loader saves clear SLOT
  coop-loader saves import PATH
  coop-loader profiles list
  coop-loader profiles switch NAME
  coop-loader profiles create NAME
  coop-loader profiles delete NAME
  coop-loader game detect
  coop-loader game set PATH
  coop-loader game play
`)
}

func service() (*app.Service, error) {
	return app.NewDefault()
}

// --- serve -----------------------------------------------------------------

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8787", "listen address")
	open := fs.Bool("open", true, "open the browser")
	_ = fs.Parse(args)

	svc, err := service()
	if err != nil {
		return err
	}
	defer svc.Close()
	srv, err := server.New(svc)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("http://%s/?token=%s", ln.Addr().String(), srv.Token())
	fmt.Printf("Coop Loader %s\n", app.Version)
	fmt.Printf("Game data: %s\n", svc.GameDataDir())
	fmt.Printf("Open:      %s\n", url)
	fmt.Println("Press Ctrl+C to stop.")

	if *open {
		openBrowser(url)
	}

	httpServer := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutCtx)
	}()

	err = httpServer.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

// --- status ----------------------------------------------------------------

func status() error {
	svc, err := service()
	if err != nil {
		return err
	}
	st := svc.Status()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "Version\t%s\n", st.Version)
	fmt.Fprintf(w, "Game folder\t%s\n", orDash(st.GamePath))
	fmt.Fprintf(w, "Game found\t%v\n", st.GameFound)
	fmt.Fprintf(w, "Game running\t%v\n", st.Running)
	fmt.Fprintf(w, "Game data\t%s\n", st.GameData)
	fmt.Fprintf(w, "Config\t%s\n", withMissing(st.ConfigPath, st.ConfigFound))
	fmt.Fprintf(w, "Save file\t%s\n", withMissing(st.SavePath, st.SaveFound))
	fmt.Fprintf(w, "Loader data\t%s\n", st.LoaderDir)
	return w.Flush()
}

// --- mods ------------------------------------------------------------------

func modsCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: coop-loader mods list|on|off|install|remove")
	}
	svc, err := service()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		list := svc.Mods()
		if len(list) == 0 {
			fmt.Println("No mods installed.")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, " \tKIND\tID\tNAME\tORIGIN")
		for _, m := range list {
			mark := " "
			if m.Enabled {
				mark = "*"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", mark, m.Kind, m.ID, m.Name, m.Origin)
		}
		return w.Flush()

	case "on", "off":
		rest, flags := parseToggles(args[1:], "pack")
		if len(rest) != 1 {
			return errors.New("usage: coop-loader mods on|off ID [--pack]")
		}
		kind := mods.KindMod
		if flags["pack"] {
			kind = mods.KindPack
		}
		disabled, err := svc.Toggle(rest[0], kind, args[0] == "on")
		if err != nil {
			return err
		}
		for _, name := range disabled {
			fmt.Printf("turned off %s: it cannot run together\n", name)
		}
		return nil

	case "install":
		rest, flags := parseToggles(args[1:], "allow-compiled")
		if len(rest) != 1 {
			return errors.New("usage: coop-loader mods install PATH [--allow-compiled]")
		}
		installed, err := svc.Install(rest[0], flags["allow-compiled"])
		if err != nil {
			return err
		}
		fmt.Println("installed:", installed)
		return nil

	case "remove":
		rest, flags := parseToggles(args[1:], "pack")
		if len(rest) != 1 {
			return errors.New("usage: coop-loader mods remove ID [--pack]")
		}
		kind := mods.KindMod
		if flags["pack"] {
			kind = mods.KindPack
		}
		moved, err := svc.Remove(rest[0], kind)
		if err != nil {
			return err
		}
		fmt.Println("moved to", moved)
		return nil
	}
	return fmt.Errorf("unknown mods command %q", args[0])
}

// --- saves -----------------------------------------------------------------

func savesCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: coop-loader saves list|copy|clear|import")
	}
	svc, err := service()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		state, err := svc.Saves()
		if err != nil {
			return err
		}
		fmt.Println("file:", withMissing(state.Path, state.Exists))
		for _, slot := range state.Slots {
			name := state.Names[slot.Index]
			if name == "" {
				name = "MARIO " + string(rune('A'+slot.Index))
			}
			if slot.Valid && slot.Exists {
				fmt.Printf("%c  %-20s %3d stars  %3d coins  cap %d/%d  [%s]\n",
					'A'+slot.Index, name, slot.Stars, slot.Coins, slot.CapLevel, slot.CapArea, slot.Signature)
			} else {
				fmt.Printf("%c  %-20s empty\n", 'A'+slot.Index, name)
			}
		}
		return nil

	case "copy":
		if len(args) != 3 {
			return errors.New("usage: coop-loader saves copy FROM TO   (0-3 or A-D)")
		}
		from, err := slotIndex(args[1])
		if err != nil {
			return err
		}
		to, err := slotIndex(args[2])
		if err != nil {
			return err
		}
		return svc.CopySlot(from, to)

	case "clear":
		if len(args) != 2 {
			return errors.New("usage: coop-loader saves clear SLOT")
		}
		slot, err := slotIndex(args[1])
		if err != nil {
			return err
		}
		return svc.ClearSlot(slot)

	case "import":
		if len(args) != 2 {
			return errors.New("usage: coop-loader saves import PATH")
		}
		return svc.ImportSave(args[1])
	}
	return fmt.Errorf("unknown saves command %q", args[0])
}

func slotIndex(s string) (int, error) {
	switch s {
	case "0", "a", "A":
		return 0, nil
	case "1", "b", "B":
		return 1, nil
	case "2", "c", "C":
		return 2, nil
	case "3", "d", "D":
		return 3, nil
	}
	return 0, fmt.Errorf("slot %q is not 0-3 or A-D", s)
}

// --- profiles --------------------------------------------------------------

func profilesCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: coop-loader profiles list|switch|create|delete")
	}
	svc, err := service()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		profiles, err := svc.Profiles()
		if err != nil {
			return err
		}
		for _, name := range profiles.Names() {
			mark := " "
			if name == profiles.Active {
				mark = "*"
			}
			fmt.Printf("%s %-24s %d mods\n", mark, name, len(profiles.Sets[name]))
		}
		return nil
	case "switch", "create", "delete":
		if len(args) != 2 {
			return fmt.Errorf("usage: coop-loader profiles %s NAME", args[0])
		}
		switch args[0] {
		case "switch":
			return svc.SwitchProfile(args[1])
		case "create":
			return svc.CreateProfile(args[1])
		default:
			return svc.DeleteProfile(args[1])
		}
	}
	return fmt.Errorf("unknown profiles command %q", args[0])
}

// --- game ------------------------------------------------------------------

func gameCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: coop-loader game detect|set|play")
	}
	svc, err := service()
	if err != nil {
		return err
	}
	switch args[0] {
	case "detect":
		path := svc.DetectGame()
		if path == "" {
			return errors.New("could not find sm64coopdx")
		}
		fmt.Println(path)
		return nil
	case "set":
		if len(args) != 2 {
			return errors.New("usage: coop-loader game set PATH")
		}
		if err := svc.SetGamePath(args[1]); err != nil {
			return err
		}
		fmt.Println("saved")
		return nil
	case "play":
		return svc.Launch(nil)
	}
	return fmt.Errorf("unknown game command %q", args[0])
}

// parseToggles pulls boolean flags out of args so users can write them before
// or after the positional argument.
func parseToggles(args []string, names ...string) ([]string, map[string]bool) {
	set := map[string]bool{}
	rest := make([]string, 0, len(args))
	for _, arg := range args {
		matched := false
		for _, name := range names {
			if arg == "--"+name || arg == "-"+name {
				set[name] = true
				matched = true
				break
			}
		}
		if !matched {
			rest = append(rest, arg)
		}
	}
	return rest, set
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func withMissing(path string, found bool) string {
	if found {
		return path
	}
	return path + " (missing)"
}
