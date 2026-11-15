package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

var shellAuthEnv = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_BETAS",
	"CLAUDE_CODE_OAUTH_TOKEN",
	"_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL",
}

// cmdLogin repairs a saved profile whose token went stale, or adds a new one
// when given an email
func cmdLogin(p paths, args []string) error {
	if len(args) == 0 || len(args) > 2 {
		return fmt.Errorf("usage: ccx login <name> [email]")
	}
	name, err := profileName(args[0])
	if err != nil {
		return err
	}
	email := ""
	if len(args) == 2 {
		email = strings.TrimSpace(args[1])
	}
	prof, err := p.loadProfile(name)
	if errors.Is(err, errNoProfile) {
		return loginNew(p, name, email)
	}
	if err != nil {
		return err
	}
	return loginRepair(p, prof, email)
}

// loginNew requires the email so a mistyped profile name fails instead of
// saving a second profile
func loginNew(p paths, name, email string) error {
	if email == "" {
		return fmt.Errorf("no profile named %q; to add it: ccx login %s <email>", name, name)
	}
	live, err := signIn(email)
	if err != nil {
		return err
	}
	if !strings.EqualFold(accountEmail(live.OAuthAccount), email) {
		return fmt.Errorf("signed in as %s, not %s; nothing saved", accountEmail(live.OAuthAccount), email)
	}
	if err := p.saveProfile(newProfile(name, live)); err != nil {
		return err
	}
	fmt.Printf("added %q (%s)\n", name, accountEmail(live.OAuthAccount))
	return nil
}

func loginRepair(p paths, prof Profile, email string) error {
	if email != "" && !strings.EqualFold(email, prof.Email) {
		return fmt.Errorf("%q is %s, not %s", prof.Name, prof.Email, email)
	}
	live, err := signIn(prof.Email)
	if err != nil {
		return err
	}
	if accountUUID(live.OAuthAccount) != accountUUID(prof.Identity.OAuthAccount) {
		return fmt.Errorf("signed in as %s, but %q is %s; profile unchanged", accountEmail(live.OAuthAccount), prof.Name, prof.Email)
	}
	prof.Identity.Credentials = live.Credentials
	if err := p.saveProfile(prof); err != nil {
		return err
	}
	fmt.Printf("repaired %q (%s)\n", prof.Name, prof.Email)
	return nil
}

// signIn runs Claude Code's sign-in in a throwaway config dir so the profile is
// the only holder of the new refresh token, which is single-use
func signIn(email string) (Identity, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Identity{}, err
	}
	claude, err := findClaude(home)
	if err != nil {
		return Identity{}, err
	}
	dir, err := os.MkdirTemp("", "ccx-login-")
	if err != nil {
		return Identity{}, err
	}
	defer os.RemoveAll(dir)

	cmd := exec.Command(claude, loginArgs(email)...)
	cmd.Env = append(signInEnv(), "CLAUDE_CONFIG_DIR="+dir)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return Identity{}, fmt.Errorf("claude auth login: %w", err)
	}

	signedIn := paths{
		configJSON: filepath.Join(dir, ".claude.json"),
		credsJSON:  filepath.Join(dir, ".credentials.json"),
	}
	return signedIn.captureLive()
}

// signInEnv drops auth set by the calling shell, which often points at the proxy
// or carries another account's token and would outrank the login being made
func signInEnv() []string {
	return slices.DeleteFunc(os.Environ(), func(entry string) bool {
		key, _, _ := strings.Cut(entry, "=")
		return slices.Contains(shellAuthEnv, strings.ToUpper(key))
	})
}

// findClaude also checks the native installer's location, which shells started
// outside a login profile may not have on PATH
func findClaude(home string) (string, error) {
	for _, candidate := range []string{"claude", filepath.Join(home, ".local", "bin", "claude")} {
		path, err := exec.LookPath(candidate)
		if err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("claude not found on PATH or in ~/.local/bin")
}

func loginArgs(email string) []string {
	args := []string{"auth", "login"}
	if email == "" {
		return args
	}
	// The sign-in page decodes the value once more than Claude Code encodes it,
	// which turns a raw + into a space
	return append(args, "--email", url.QueryEscape(email))
}
