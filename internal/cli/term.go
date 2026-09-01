package cli

import "github.com/upstacked/cli/internal/ui"

func readPasswordNoEcho(fd int) (string, error) { return ui.ReadPassword(fd) }
