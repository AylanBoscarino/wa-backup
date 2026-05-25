// Package setup implements the interactive `wa-backup --setup` wizard.
// It walks a fresh (or returning) user through choosing storage paths,
// pairing with WhatsApp, picking which groups to monitor, and writing a
// usable .env file. Existing values in .env are loaded and presented as
// defaults so re-running setup is idempotent.
package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AylanBoscarino/wa-backup/internal/client"
	"github.com/charmbracelet/huh"
	"github.com/joho/godotenv"
	"go.uber.org/zap"
)

// Defaults match the documented out-of-the-box values for env vars.
const (
	defaultBackupPath      = "./backup"
	defaultSessionDBPath   = "./wa-session.db"
	defaultHistorySyncIdle = "30s"
	defaultMediaWorkers    = "4"
	defaultLogLevel        = "info"
)

// EnvPath is the location of the .env we read from and write to.
const EnvPath = ".env"

// Run is the entry point invoked by `cmd/main.go` when --setup is set.
// It expects an interactive TTY; if stdin or stdout is not a terminal the
// caller is expected to refuse before calling Run.
func Run(ctx context.Context, log *zap.SugaredLogger) error {
	if err := banner(); err != nil {
		return err
	}

	current := loadCurrentEnv(EnvPath)
	state := stateFromEnv(current)

	if err := stepStorage(&state); err != nil {
		return err
	}
	if err := ensureDirs(state); err != nil {
		return err
	}

	jids, names, err := stepPairAndFetchGroups(ctx, state, log)
	if err != nil {
		return err
	}

	selected, err := stepPickGroups(jids, names, state.AllowList)
	if err != nil {
		return err
	}
	state.AllowList = selected

	if err := writeEnv(EnvPath, state); err != nil {
		return err
	}

	printNextSteps(state)
	return nil
}

// state is a typed snapshot of what will go into .env. Strings (rather
// than int/time.Duration) so we can round-trip arbitrary user input and
// let `config.Load` validate it later.
type state struct {
	BackupPath      string
	SessionDBPath   string
	HistorySyncIdle string
	MediaWorkers    string
	LogLevel        string
	AllowList       []string
}

func loadCurrentEnv(path string) map[string]string {
	out, err := godotenv.Read(path)
	if err != nil {
		return map[string]string{}
	}
	return out
}

func stateFromEnv(env map[string]string) state {
	return state{
		BackupPath:      orDefault(env["LOCAL_BACKUP_PATH"], defaultBackupPath),
		SessionDBPath:   orDefault(env["SESSION_DB_PATH"], defaultSessionDBPath),
		HistorySyncIdle: orDefault(env["HISTORY_SYNC_IDLE"], defaultHistorySyncIdle),
		MediaWorkers:    orDefault(env["MEDIA_WORKERS"], defaultMediaWorkers),
		LogLevel:        orDefault(env["LOG_LEVEL"], defaultLogLevel),
		AllowList:       splitCSV(env["GROUP_ALLOWLIST"]),
	}
}

func banner() error {
	intro := strings.Join([]string{
		"wa-backup — interactive setup",
		"",
		"This wizard runs in 4 steps:",
		"  1. Choose where to store backups and the session key",
		"  2. Pair with WhatsApp via QR code (skipped if already paired)",
		"  3. Pick which groups to monitor",
		"  4. Write the .env file",
		"",
		"⚠  wa-backup is an unofficial WhatsApp client (whatsmeow). Meta's",
		"   Terms of Service prohibit this. Your account may be banned.",
	}, "\n")

	var ok bool
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewNote().Title("Welcome").Description(intro),
			huh.NewConfirm().Title("Continue?").Affirmative("Yes").Negative("Cancel").Value(&ok),
		),
	)
	if err := form.Run(); err != nil {
		return err
	}
	if !ok {
		return errors.New("setup canceled by user")
	}
	return nil
}

func stepStorage(s *state) error {
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewNote().Title("Step 1 of 4 — Storage"),
			huh.NewInput().
				Title("Backup directory").
				Description("Absolute path is recommended so launchd/cron always finds it.").
				Value(&s.BackupPath).
				Validate(notEmpty("backup directory")),
			huh.NewInput().
				Title("Session DB path").
				Description("Holds the cryptographic keys that link this daemon to your WhatsApp.").
				Value(&s.SessionDBPath).
				Validate(notEmpty("session DB path")),
			huh.NewInput().
				Title("History sync idle timeout").
				Description("How long to wait without new batches before declaring history sync complete.").
				Value(&s.HistorySyncIdle).
				Validate(validateDuration),
			huh.NewInput().
				Title("Media workers").
				Description("Parallel download goroutines.").
				Value(&s.MediaWorkers).
				Validate(validatePositiveInt),
			huh.NewSelect[string]().
				Title("Log level").
				Options(
					huh.NewOption("info — default", "info"),
					huh.NewOption("debug — verbose, shows every message", "debug"),
					huh.NewOption("warn", "warn"),
					huh.NewOption("error", "error"),
				).
				Value(&s.LogLevel),
		),
	)
	return form.Run()
}

func ensureDirs(s state) error {
	if err := os.MkdirAll(s.BackupPath, 0o700); err != nil {
		return fmt.Errorf("create backup dir: %w", err)
	}
	if dir := filepath.Dir(s.SessionDBPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create session dir: %w", err)
		}
	}
	return nil
}

// stepPairAndFetchGroups pairs the daemon (skipping QR if already paired)
// and returns the list of joined groups. Returns parallel slices keyed by
// index so the multiselect can use string keys (JIDs are comparable).
func stepPairAndFetchGroups(ctx context.Context, s state, log *zap.SugaredLogger) ([]string, []string, error) {
	fmt.Println()
	fmt.Println("Step 2 of 4 — Pair with WhatsApp")
	fmt.Println(strings.Repeat("─", 32))

	waClient, err := client.New(ctx, s.SessionDBPath, log)
	if err != nil {
		return nil, nil, fmt.Errorf("init client: %w", err)
	}

	if waClient.IsPaired() {
		fmt.Println("✓ Session already paired — skipping QR step.")
	} else {
		fmt.Println("On your phone open: WhatsApp → Settings → Linked devices → Link a device")
		fmt.Println("Then scan the QR code that will appear below.")
		fmt.Println()
	}

	pairCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := waClient.PairIfNeeded(pairCtx); err != nil {
		return nil, nil, fmt.Errorf("pair: %w", err)
	}
	defer waClient.Shutdown()

	cli := waClient.Underlying()
	if !cli.WaitForConnection(20 * time.Second) {
		return nil, nil, fmt.Errorf("connection did not complete within 20s")
	}

	fmt.Println()
	fmt.Println("Step 3 of 4 — Pick groups")
	fmt.Println(strings.Repeat("─", 25))
	fmt.Print("Fetching joined groups… ")
	groups, err := cli.GetJoinedGroups(ctx)
	if err != nil {
		fmt.Println("failed")
		return nil, nil, fmt.Errorf("get joined groups: %w", err)
	}
	fmt.Printf("found %d.\n\n", len(groups))

	// Sort alphabetically — a deterministic order is friendlier than the
	// server's response order for a checkbox picker.
	sort.Slice(groups, func(i, j int) bool {
		return strings.ToLower(groups[i].Name) < strings.ToLower(groups[j].Name)
	})

	jids := make([]string, len(groups))
	labels := make([]string, len(groups))
	for i, g := range groups {
		jids[i] = g.JID.String()
		labels[i] = fmt.Sprintf("%-40s %3d members", truncate(g.Name, 40), g.ParticipantCount)
	}
	return jids, labels, nil
}

func stepPickGroups(jids, labels []string, preselected []string) ([]string, error) {
	if len(jids) == 0 {
		return nil, errors.New("you don't appear to be a member of any groups")
	}

	already := map[string]struct{}{}
	for _, j := range preselected {
		already[j] = struct{}{}
	}

	options := make([]huh.Option[string], len(jids))
	for i, jid := range jids {
		opt := huh.NewOption(labels[i], jid)
		if _, ok := already[jid]; ok {
			opt = opt.Selected(true)
		}
		options[i] = opt
	}

	var picked []string
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("Which groups should wa-backup monitor?").
				Description("Space toggles, ↑/↓ moves, / filters, Enter confirms. Pick at least one.").
				Options(options...).
				Filterable(true).
				Height(15).
				Value(&picked).
				Validate(func(v []string) error {
					if len(v) == 0 {
						return errors.New("select at least one group")
					}
					return nil
				}),
		),
	)
	if err := form.Run(); err != nil {
		return nil, err
	}
	return picked, nil
}

func writeEnv(path string, s state) error {
	contents := strings.Join([]string{
		"# wa-backup — generated by `wa-backup --setup`",
		"# Re-run --setup any time to update these values.",
		"",
		"LOCAL_BACKUP_PATH=" + s.BackupPath,
		"SESSION_DB_PATH=" + s.SessionDBPath,
		"GROUP_ALLOWLIST=" + strings.Join(s.AllowList, ","),
		"GROUP_DENYLIST=",
		"MEDIA_WORKERS=" + s.MediaWorkers,
		"LOG_LEVEL=" + s.LogLevel,
		"HISTORY_SYNC_IDLE=" + s.HistorySyncIdle,
		"",
	}, "\n")

	// Back up any existing .env before overwriting. The user explicitly
	// re-ran setup, but a one-off mistake shouldn't destroy their config.
	if _, err := os.Stat(path); err == nil {
		backup := fmt.Sprintf("%s.bak.%s", path, time.Now().Format("20060102-150405"))
		if err := os.Rename(path, backup); err == nil {
			fmt.Printf("Previous .env backed up to %s\n", backup)
		}
	}
	return os.WriteFile(path, []byte(contents), 0o600)
}

func printNextSteps(s state) {
	fmt.Println()
	fmt.Println("Step 4 of 4 — Done")
	fmt.Println(strings.Repeat("─", 18))
	fmt.Println("Configuration written to .env. Selected groups:")
	for _, j := range s.AllowList {
		fmt.Println("  •", j)
	}
	fmt.Println()
	fmt.Println("Start the daemon:")
	fmt.Println("  wa-backup")
	fmt.Println()
	fmt.Println("Inspect groups any time:")
	fmt.Println("  wa-backup --list-groups")
	fmt.Println()
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func notEmpty(field string) func(string) error {
	return func(v string) error {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%s cannot be empty", field)
		}
		return nil
	}
}

func validateDuration(v string) error {
	if _, err := time.ParseDuration(strings.TrimSpace(v)); err != nil {
		return fmt.Errorf("not a duration (e.g. 30s, 2m): %w", err)
	}
	return nil
}

func validatePositiveInt(v string) error {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return fmt.Errorf("not a number")
	}
	if n <= 0 {
		return fmt.Errorf("must be positive")
	}
	return nil
}

func truncate(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	rs := []rune(s)
	return string(rs[:n-1]) + "…"
}

// IsTTY reports whether the given file is connected to a terminal. Used
// by `cmd/main.go` to fail loudly when --setup is invoked in a pipe.
func IsTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

