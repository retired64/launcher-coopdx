package app

import (
	"errors"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
)

// OpenExternal opens an https link in the user's browser.
//
// Only https is accepted: the loader never hands the shell a file path or a
// custom scheme coming from a mod page.
func (s *Service) OpenExternal(raw string) error {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("only https links can be opened")
	}

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u.String())
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u.String())
	default:
		cmd = exec.Command("xdg-open", u.String())
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
