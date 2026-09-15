// Package cli is the shell's terminal interface.
//
// Settings live here rather than in a window because the shell has no widget
// toolkit: it draws its own overlay, which needs no text fields, and building
// one for a settings dialog would be the largest and least interesting part of
// the Linux port. Everything enumerable — provider, endpoint, model, thinking
// level, preset — is also offered in the tray menu, built from the same table.
// This is where the things you have to type live, and it is the only route for
// the API key, which must never appear in a command line.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/srikary12/starch/apps/desktop/internal/secret"
	"github.com/srikary12/starch/apps/desktop/internal/settings"
	"github.com/srikary12/starch/internal/catalog"
)

// SecretStore is the part of the keyring this command uses. An interface so
// the tests do not need a keyring, and so a failure to reach one is only
// reported when a key operation was actually asked for.
type SecretStore interface {
	Set(account, value string) error
	Get(account string) (string, bool, error)
	Has(account string) (bool, error)
	Delete(account string) error
}

// Env is everything the command touches, so a test can supply all of it.
type Env struct {
	Stdout io.Writer
	Stderr io.Writer

	// Store holds the non-secret preferences.
	Store *settings.Store
	// Catalog is the provider and model table.
	//
	// The linked copy, not one fetched from the daemon: this command has to
	// work before anything is configured and with no daemon running, which is
	// exactly the situation the built-in catalog exists for. The running shell
	// refreshes from GET /v1/models, where a newer daemon can offer more.
	Catalog catalog.Catalog

	// OpenSecrets connects to the keyring. Called only when a key operation
	// was requested, so that reading your settings never needs a keyring.
	OpenSecrets func() (SecretStore, func() error, error)
	// ReadSecret prompts for a secret without echoing it.
	ReadSecret func(prompt string) (string, error)
}

// Run executes `starch config`.
func Run(args []string, env Env) error {
	flags := flag.NewFlagSet("starch config", flag.ContinueOnError)
	flags.SetOutput(env.Stderr)

	var (
		provider = flags.String("provider", "", "provider id; adopts its endpoint, model and default thinking level")
		endpoint = flags.String("endpoint", "", "base URL to send rewrites to")
		model    = flags.String("model", "", "model id; any value is accepted, the list is a convenience")
		effort   = flags.String("effort", "", `thinking level, or "off" to leave the endpoint's own default`)
		preset   = flags.String("preset", "", "rewriting style to use by default")
		hotkey   = flags.String("hotkey", "", `global shortcut, e.g. "Ctrl+Alt+Super+P"`)
		debug    = flags.String("debug", "", "on or off; verbose logging from the helper")
		setKey   = flags.Bool("key", false, "read an API key from the terminal and store it in the keyring")
		clearKey = flags.Bool("clear-key", false, "remove the stored API key for the current provider")
		list     = flags.Bool("list", false, "print the providers, endpoints and models on offer")
	)
	flags.Usage = func() {
		fmt.Fprintf(env.Stderr, "usage: starch config [flags]\n\n"+
			"With no flags, prints the current configuration.\n\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}

	if *list {
		printCatalog(env)
		return nil
	}

	prefs, err := env.Store.Load()
	if err != nil {
		// Load always returns something usable. Say what was wrong and carry
		// on, rather than refusing to let someone fix their settings.
		fmt.Fprintf(env.Stderr, "warning: %v\n", err)
	}

	changed := false

	if *provider != "" {
		if settings.FindProvider(env.Catalog, *provider) == nil {
			return fmt.Errorf("no such provider %q. Try one of: %s",
				*provider, strings.Join(providerIDs(env.Catalog), ", "))
		}
		prefs.UseProvider(env.Catalog, *provider)
		changed = true
	}
	if *endpoint != "" {
		prefs.BaseURL = *endpoint
		changed = true
	}
	if *model != "" {
		prefs.Model = *model
		// The level that was set belonged to the previous model. Re-seed from
		// the new one, or clear it if this model has no reasoning controls.
		prefs.UseDefaultEffort(env.Catalog)
		changed = true
	}
	if *effort != "" {
		level, err := resolveEffort(env, prefs, *effort)
		if err != nil {
			return err
		}
		prefs.ThinkingEffort = level
		changed = true
	}
	if *preset != "" {
		prefs.PresetID = *preset
		changed = true
	}
	if *hotkey != "" {
		parsed, err := settings.ParseHotKey(*hotkey)
		if err != nil {
			return err
		}
		if reason := parsed.InvalidReason(); reason != "" {
			return errors.New(reason)
		}
		prefs.HotKey = parsed
		changed = true
	}
	if *debug != "" {
		on, err := onOff(*debug)
		if err != nil {
			return err
		}
		prefs.Debug = on
		changed = true
	}

	if changed {
		if err := env.Store.Save(prefs); err != nil {
			return err
		}
	}

	if *setKey || *clearKey {
		if err := changeKey(env, prefs.Provider, *setKey); err != nil {
			return err
		}
	}

	printSettings(env, prefs)
	return nil
}

// resolveEffort turns what was typed into a level to send, refusing one the
// chosen model is known not to accept.
func resolveEffort(env Env, prefs settings.Preferences, requested string) (string, error) {
	if requested == "off" {
		return "", nil
	}

	model := settings.FindModel(env.Catalog, prefs.Provider, prefs.BaseURL, prefs.Model)
	if model == nil {
		// A model we know nothing about — typed in, or newer than this build.
		// Passing the level through is the only option that does not make a
		// stale table into a restriction.
		return requested, nil
	}
	if model.Thinking == nil {
		return "", fmt.Errorf("%s has no thinking setting", prefs.Model)
	}
	for _, level := range model.Thinking.Levels {
		if string(level) == requested {
			return requested, nil
		}
	}
	return "", fmt.Errorf("%s does not accept %q. It takes: %s",
		prefs.Model, requested, joinEfforts(model.Thinking.Levels))
}

// changeKey stores or removes the API key for a provider.
func changeKey(env Env, providerID string, set bool) error {
	store, closeStore, err := env.OpenSecrets()
	if err != nil {
		return err
	}
	defer func() { _ = closeStore() }()

	account := secret.Account(providerID)
	if !set {
		if err := store.Delete(account); err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "Removed the %s key from the keyring.\n", providerID)
		return nil
	}

	// Read rather than accept as an argument: a command line is visible to
	// every process on the machine through ps, and shells write it to history.
	value, err := env.ReadSecret(fmt.Sprintf("%s API key: ", providerID))
	if err != nil {
		return err
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("no key was entered")
	}
	if err := store.Set(account, value); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "Stored the %s key in the keyring.\n", providerID)
	return nil
}

func printSettings(env Env, prefs settings.Preferences) {
	out := env.Stdout
	fmt.Fprintf(out, "Provider   %s\n", describeProvider(env.Catalog, prefs.Provider))
	fmt.Fprintf(out, "Endpoint   %s\n", prefs.BaseURL)
	fmt.Fprintf(out, "Model      %s\n", describeModel(env.Catalog, prefs))
	if prefs.ThinkingEffort == "" {
		fmt.Fprintf(out, "Thinking   the endpoint's own default\n")
	} else {
		fmt.Fprintf(out, "Thinking   %s\n", prefs.ThinkingEffort)
	}
	fmt.Fprintf(out, "Preset     %s\n", prefs.PresetID)
	fmt.Fprintf(out, "Shortcut   %s\n", prefs.HotKey)
	fmt.Fprintf(out, "API key    %s\n", describeKey(env, prefs.Provider))
	fmt.Fprintf(out, "Settings   %s\n", env.Store.Path())

	// Last, and unmissable: a configuration that cannot be used yet says so
	// here rather than at the moment someone tries to rewrite something.
	if problem := prefs.Problem(env.Catalog); problem != nil {
		fmt.Fprintf(out, "\nNot ready: %v\n", problem)
	}
}

// describeKey reports whether a key is stored without reading it, and without
// treating a missing keyring as a failure of the whole command.
func describeKey(env Env, providerID string) string {
	provider := settings.FindProvider(env.Catalog, providerID)
	needed := provider == nil || provider.RequiresKey

	store, closeStore, err := env.OpenSecrets()
	if err != nil {
		return "unknown (" + err.Error() + ")"
	}
	defer func() { _ = closeStore() }()

	has, err := store.Has(secret.Account(providerID))
	switch {
	case err != nil:
		return "unknown (" + err.Error() + ")"
	case has:
		return "saved in the keyring"
	case !needed:
		return "none, which this endpoint does not need"
	default:
		return "not set — run `starch config --key`"
	}
}

func describeProvider(cat catalog.Catalog, id string) string {
	if provider := settings.FindProvider(cat, id); provider != nil {
		return fmt.Sprintf("%s (%s)", provider.Name, id)
	}
	return id
}

func describeModel(cat catalog.Catalog, prefs settings.Preferences) string {
	if prefs.Model == "" {
		// Not a gap in the table: an endpoint serving whatever a machine has
		// pulled cannot have a default, and the note says what to type.
		return "not set"
	}
	model := settings.FindModel(cat, prefs.Provider, prefs.BaseURL, prefs.Model)
	if model == nil {
		return prefs.Model
	}
	return fmt.Sprintf("%s (%s)", model.Name, model.ID)
}

// printCatalog is what a dropdown would have shown.
func printCatalog(env Env) {
	out := env.Stdout
	for _, provider := range env.Catalog.Providers {
		key := "no key needed"
		if provider.RequiresKey {
			key = "needs an API key"
		}
		fmt.Fprintf(out, "%s  (--provider %s, %s)\n", provider.Name, provider.ID, key)

		for _, endpoint := range provider.Endpoints {
			fmt.Fprintf(out, "  %s\n", endpoint.URL)
			if endpoint.Note != "" {
				fmt.Fprintf(out, "    %s\n", endpoint.Note)
			}
			for _, model := range endpoint.Models {
				marker := " "
				if model.ID == endpoint.DefaultModel {
					marker = "*"
				}
				line := fmt.Sprintf("   %s %-28s %s", marker, model.ID, model.Name)
				if model.Thinking != nil {
					line += "  [" + joinEfforts(model.Thinking.Levels) + "]"
				}
				fmt.Fprintln(out, line)
				if model.Note != "" {
					fmt.Fprintf(out, "       %s\n", model.Note)
				}
			}
		}
		fmt.Fprintln(out)
	}
	fmt.Fprintln(out, "* is the default. Any model id is accepted — the list is a convenience,")
	fmt.Fprintln(out, "not a restriction, so a model newer than this build is still reachable.")
}

func joinEfforts(levels []catalog.Effort) string {
	parts := make([]string, len(levels))
	for i, level := range levels {
		parts[i] = string(level)
	}
	return strings.Join(parts, ", ")
}

func providerIDs(cat catalog.Catalog) []string {
	ids := make([]string, len(cat.Providers))
	for i, provider := range cat.Providers {
		ids[i] = provider.ID
	}
	return ids
}

func onOff(value string) (bool, error) {
	switch strings.ToLower(value) {
	case "on", "true", "yes", "1":
		return true, nil
	case "off", "false", "no", "0":
		return false, nil
	}
	return false, fmt.Errorf("expected on or off, got %q", value)
}
