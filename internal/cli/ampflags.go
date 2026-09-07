package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/amp"
	"github.com/spf13/cobra"
)

// ampFlags collects the connection details shared by every command that talks
// to a panel.
type ampFlags struct {
	url          string
	user         string
	passwordFile string
	twoFactor    string
	insecure     bool
	timeout      time.Duration
}

func (a *ampFlags) register(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&a.url, "amp-url", envOr("AMPBB_AMP_URL", ""),
		"AMP panel or instance URL, e.g. http://127.0.0.1:8080 (env AMPBB_AMP_URL)")
	f.StringVar(&a.user, "amp-user", envOr("AMPBB_AMP_USER", ""),
		"AMP username (env AMPBB_AMP_USER)")
	f.StringVar(&a.passwordFile, "amp-password-file", envOr("AMPBB_AMP_PASSWORD_FILE", ""),
		"file holding the AMP password; prefer this over the environment")
	f.StringVar(&a.twoFactor, "amp-2fa", envOr("AMPBB_AMP_2FA", ""),
		"current two-factor code, if the account requires one")
	f.BoolVar(&a.insecure, "amp-insecure", false,
		"skip TLS verification for the panel (only for a self-signed certificate)")
	f.DurationVar(&a.timeout, "amp-timeout", 30*time.Second, "per-request timeout for the AMP API")
}

// configured reports whether enough was given to attempt a connection.
func (a *ampFlags) configured() bool { return a.url != "" }

// password resolves the credential without ever putting it on a command line.
func (a *ampFlags) password() (string, error) {
	if a.passwordFile != "" {
		raw, err := os.ReadFile(a.passwordFile)
		if err != nil {
			return "", fmt.Errorf("reading --amp-password-file: %w", err)
		}
		return strings.TrimRight(string(raw), "\r\n"), nil
	}
	if p := os.Getenv("AMPBB_AMP_PASSWORD"); p != "" {
		return p, nil
	}
	return "", errors.New("no AMP password: set AMPBB_AMP_PASSWORD or pass --amp-password-file")
}

func (a *ampFlags) client() (*amp.Client, error) {
	if !a.configured() {
		return nil, errors.New("no --amp-url given")
	}
	if a.user == "" {
		return nil, errors.New("no --amp-user given")
	}
	pw, err := a.password()
	if err != nil {
		return nil, err
	}
	return amp.New(amp.Config{
		BaseURL:            a.url,
		Username:           a.user,
		Password:           pw,
		TwoFactorToken:     a.twoFactor,
		Timeout:            a.timeout,
		InsecureSkipVerify: a.insecure,
	})
}

// resolveRoot asks AMP where an instance lives, so that a datastore layout
// this tool has never seen still works and no path is guessed.
func resolveRoot(ctx context.Context, c *amp.Client, instance string) (string, error) {
	instances, err := c.GetLocalInstances(ctx)
	if err != nil {
		return "", fmt.Errorf("listing instances: %w", err)
	}
	var names []string
	for _, in := range instances {
		names = append(names, in.InstanceName)
		if !strings.EqualFold(in.InstanceName, instance) && !strings.EqualFold(in.FriendlyName, instance) {
			continue
		}
		dir := in.Directory()
		if dir == "" {
			return "", fmt.Errorf("AMP reported instance %q but no directory for it; pass --root explicitly",
				instance)
		}
		return dir, nil
	}
	return "", fmt.Errorf("AMP does not know an instance named %q (it lists: %s)",
		instance, strings.Join(names, ", "))
}
