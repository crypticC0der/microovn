package inference

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/canonical/microovn/microovn/ovn/paths"
	"github.com/canonical/microovn/microovn/snap"
)

// Active returns the name of the currently selected engine, or an empty string
// when no engine has been selected yet.
func Active() (string, error) {
	data, err := os.ReadFile(paths.ActiveEngineFile())
	if os.IsNotExist(err) {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("reading active engine: %w", err)
	}

	return strings.TrimSpace(string(data)), nil
}

// Activate records the selected engine and installs the snap components it
// provides. Only the components required by the selected engine are installed;
// the switch service consults the recorded engine to pick the right binaries.
func Activate(ctx context.Context, engine *Engine) error {
	err := os.MkdirAll(paths.InferenceDir(), 0755)
	if err != nil {
		return fmt.Errorf("creating inference state directory: %w", err)
	}

	err = os.WriteFile(paths.ActiveEngineFile(), []byte(engine.Name+"\n"), 0644)
	if err != nil {
		return fmt.Errorf("recording active engine: %w", err)
	}

	for _, component := range engine.Components {
		err = snap.InstallComponent(ctx, component)
		if err != nil {
			return fmt.Errorf("installing component %q: %w", component, err)
		}
	}

	return nil
}
