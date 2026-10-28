package inference

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// LoadEngines reads every engine manifest (*.yaml or *.yml) from dir and
// returns them sorted by name.
func LoadEngines(dir string) ([]Engine, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading engine directory %q: %w", dir, err)
	}

	engines := make([]Engine, 0, len(entries))

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		ext := filepath.Ext(entry.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}

		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading engine manifest %q: %w", entry.Name(), err)
		}

		var engine Engine
		err = yaml.Unmarshal(data, &engine)
		if err != nil {
			return nil, fmt.Errorf("parsing engine manifest %q: %w", entry.Name(), err)
		}

		if engine.Name == "" {
			return nil, fmt.Errorf("engine manifest %q is missing a name", entry.Name())
		}

		engines = append(engines, engine)
	}

	sort.Slice(engines, func(i, j int) bool {
		return engines[i].Name < engines[j].Name
	})

	return engines, nil
}
