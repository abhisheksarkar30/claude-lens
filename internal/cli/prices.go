package cli

import (
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"

	"github.com/abhisheksarkar30/claude-lens/internal/pricing"
)

// Prices is `clens prices`: the effective rate table, plus the edit paths.
//
// "Effective" is the loaded table, not the override file: a model shows the
// rate that will actually be charged, whether it came from the shipped table
// or from a user override, and the SOURCE column says which. A model with no
// rate at all is listed as unpriced rather than given a zero.
func Prices(args []string) error {
	return runPrices(args, os.Stdout)
}

func runPrices(args []string, w io.Writer) error {
	sets, rest := collectFlag(args, "--set")
	unsets, rest := collectFlag(rest, "--unset")
	edit, rest := hasFlag(rest, "--edit")

	path := pricing.DefaultPath()

	// Every edit is parsed and applied to the in-memory table before anything
	// is written, so a typo in the third --set leaves the file untouched
	// rather than half-applied.
	if len(sets) > 0 || len(unsets) > 0 {
		if err := applyPriceEdits(path, sets, unsets, w); err != nil {
			return err
		}
	}

	if edit {
		if err := runEditor(path, w); err != nil {
			return err
		}
	}

	// The table is printed after any edit, so `clens prices --set …` shows
	// the result of what it just did rather than the state before it.
	rates := pricing.NewLoader(path, nil).Table()
	if len(rates) == 0 {
		fmt.Fprintln(w, "no rates available")
		return nil
	}

	names := make([]string, 0, len(rates))
	for model := range rates {
		names = append(names, model)
	}
	sort.Strings(names)

	rows := make([][]string, 0, len(names))
	for _, model := range names {
		r := rates[model]
		rows = append(rows, []string{
			model,
			r.Source,
			mtok(r.InputRate),
			mtok(r.OutputRate),
			mtok(r.CacheWrite5mRate),
			mtok(r.CacheWrite1hRate),
			mtok(r.CacheReadRate),
		})
	}
	fmt.Fprint(w, table(
		[]string{"MODEL", "SOURCE", "IN", "OUT", "CACHE-W5M", "CACHE-W1H", "CACHE-R"},
		rows,
	))
	fmt.Fprintf(w, "\nrates are USD per million tokens; overrides live in %s\n", path)
	return nil
}

// applyPriceEdits writes sets and unsets to path. Each --set merges onto the
// model's *effective* rate, so `--set claude-sonnet-5:output_rate=18` changes
// one field and leaves input, cache and the rest exactly as they were --
// writing only the named field would leave every other class nil, and a nil
// rate computes a zero cost, which is the one outcome this repo never
// invents (invariant 5).
func applyPriceEdits(path string, sets, unsets []string, w io.Writer) error {
	overrides, err := pricing.LoadOverrides(path)
	if err != nil {
		return fmt.Errorf("prices: %w", err)
	}
	effective := pricing.NewLoader(path, nil).Table()

	for _, s := range sets {
		model, key, value, err := splitSet(s)
		if err != nil {
			return fmt.Errorf("prices: %w", err)
		}
		r, ok := effective[model]
		if !ok {
			// A model with no shipped rate can still be given one, but say so:
			// the user is defining a rate, not adjusting one.
			fmt.Fprintf(w, "prices: %s has no shipped rate; defining it from scratch\n", model)
			r = pricing.Rate{Model: model}
		}
		if err := setRateField(&r, key, value); err != nil {
			return fmt.Errorf("prices: %s: %w", model, err)
		}
		r.Model = model
		overrides[model] = r
	}

	for _, model := range unsets {
		delete(overrides, model)
	}

	if err := pricing.SaveOverrides(path, overrides); err != nil {
		return fmt.Errorf("prices: %w", err)
	}
	for _, s := range sets {
		fmt.Fprintf(w, "set %s\n", s)
	}
	for _, model := range unsets {
		fmt.Fprintf(w, "unset %s\n", model)
	}
	return nil
}

// splitSet parses `model:key=value`. The model may itself contain colons
// (a dated model id), so the two separators are found from the right: the
// last '=' splits the value, and the last ':' before it splits the model.
func splitSet(s string) (model, key, value string, err error) {
	eq := strings.LastIndex(s, "=")
	if eq < 0 {
		return "", "", "", fmt.Errorf("invalid --set %q (want model:key=value)", s)
	}
	head, value := s[:eq], s[eq+1:]
	colon := strings.LastIndex(head, ":")
	if colon < 0 {
		return "", "", "", fmt.Errorf("invalid --set %q (want model:key=value)", s)
	}
	model, key = head[:colon], head[colon+1:]
	if model == "" || key == "" || value == "" {
		return "", "", "", fmt.Errorf("invalid --set %q (want model:key=value)", s)
	}
	return model, key, value, nil
}

// setRateField assigns one override-file key on r, converting the value from
// USD-per-million-tokens (how the file and this command both express rates)
// to the per-token big.Rat the Rate stores.
//
// The seven key names are internal/pricing's rateFields, which is unexported.
// Duplicating them is smaller than exporting a setter for one CLI's use, and
// the default case means a key pricing adds later is rejected loudly here
// rather than silently ignored.
func setRateField(r *pricing.Rate, key, value string) error {
	rat, ok := new(big.Rat).SetString(value)
	if !ok {
		return fmt.Errorf("invalid rate %q (want a decimal USD-per-million-tokens)", value)
	}
	perToken := rat.Quo(rat, big.NewRat(1_000_000, 1))
	switch key {
	case "input_rate":
		r.InputRate = perToken
	case "output_rate":
		r.OutputRate = perToken
	case "cache_read_rate":
		r.CacheReadRate = perToken
	case "cache_write_5m_rate":
		r.CacheWrite5mRate = perToken
	case "cache_write_1h_rate":
		r.CacheWrite1hRate = perToken
	case "fast_input_rate":
		r.FastInputRate = perToken
	case "fast_output_rate":
		r.FastOutputRate = perToken
	default:
		return fmt.Errorf("unknown rate key %q (want input_rate, output_rate, cache_read_rate, cache_write_5m_rate, cache_write_1h_rate, fast_input_rate or fast_output_rate)", key)
	}
	return nil
}

// mtok renders a per-token rate back as USD per million tokens, in the same
// units the override file uses. A nil rate is an em dash: the class is not
// priced for this model, which is not the same as a rate of zero.
func mtok(r *big.Rat) string {
	if r == nil {
		return "—"
	}
	return new(big.Rat).Mul(r, big.NewRat(1_000_000, 1)).FloatString(4)
}

// runEditor opens path in $EDITOR (or the platform's default editor when
// EDITOR is unset), connected to this process's own terminal. The output
// writer is handed to the editor's stdout so `clens prices --edit | tee`
// captures what it printed.
func runEditor(path string, w io.Writer) error {
	// Create the file first: an editor handed a path that does not exist
	// opens an empty buffer it may refuse to save.
	if err := pricing.SaveOverrides(path, loadOrEmpty(path)); err != nil {
		return fmt.Errorf("prices: %w", err)
	}
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
		if runtime.GOOS == "windows" {
			editor = "notepad"
		}
	}
	cmd := exec.Command(editor, path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = w
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("prices: %s %s: %w", editor, path, err)
	}
	return nil
}

func loadOrEmpty(path string) pricing.Table {
	t, err := pricing.LoadOverrides(path)
	if err != nil {
		return pricing.Table{}
	}
	return t
}
