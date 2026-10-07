package appconfig

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultPath = "configs/config.yml"
	PathEnvVar  = "CONFIG_PATH"
)

// Duration exists because yaml.v3 decodes time.Duration as an integer count of
// nanoseconds, which is unusable in a hand-edited file.
type Duration time.Duration

func (d Duration) Std() time.Duration {
	return time.Duration(d)
}

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\": %w", err)
	}

	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("%w: %q is not a duration", ErrInvalidConfig, raw)
	}

	*d = Duration(parsed)

	return nil
}

func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// Path resolves the config location: CONFIG_PATH when set, otherwise the default.
// The second return says whether the caller chose it, which decides whether a
// missing file is fatal.
func Path() (string, bool) {
	if custom := os.Getenv(PathEnvVar); custom != "" {
		return custom, true
	}

	return DefaultPath, false
}

// Load layers the file over Default(), so a partial file only overrides the keys
// it names. path may list several files, comma-separated, each layered over the
// ones before it. A missing file yields ErrNotFound with usable defaults returned.
func Load(path string) (Config, error) {
	cfg := Default()

	decoded := cfg

	for _, file := range strings.Split(path, ",") {
		file = strings.TrimSpace(file)

		raw, err := os.ReadFile(file)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return cfg, fmt.Errorf("%w: %s", ErrNotFound, file)
			}

			return cfg, fmt.Errorf("reading %s: %w", file, err)
		}

		if err := yaml.Unmarshal(raw, &decoded); err != nil {
			return cfg, fmt.Errorf("parsing %s: %w", file, err)
		}
	}

	if err := decoded.Validate(); err != nil {
		return cfg, err
	}

	return decoded, nil
}
