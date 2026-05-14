// Package config parses and validates the northwatch.yaml file
// describing watched components. The package is a leaf: it depends
// only on the standard library and gopkg.in/yaml.v3. Translation
// into store types happens in cmd/northwatch.
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// File is the top-level shape of northwatch.yaml.
type File struct {
	Components []Spec `yaml:"components"`
}

// Spec is one declared component. DisplayName is optional; the
// caller defaults it to Name when empty (the package does not
// mutate Spec).
type Spec struct {
	Kind        string `yaml:"kind"`
	Namespace   string `yaml:"namespace"`
	Name        string `yaml:"name"`
	DisplayName string `yaml:"displayName"`
}

// Load reads path, parses it as YAML, runs Validate, and returns
// the resulting *File. I/O and parse failures are wrapped with the
// path (e.g. `config: open "x.yaml": ...`). Validation failures
// are returned as *ValidationError unchanged — they aggregate
// per-rule problems and don't carry the path, since the caller
// (cmd/northwatch) logs the path as a structured field alongside
// the error.
func Load(path string) (*File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: open %q: %w", path, err)
	}
	var f File
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("config: parse %q: %w", path, err)
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// ValidationError aggregates every problem found in a single
// Validate pass. The Errs slice is exported so tests (and callers)
// can assert on individual problems without string-matching.
type ValidationError struct {
	Errs []error
}

func (e *ValidationError) Error() string {
	if len(e.Errs) == 0 {
		return "config: ValidationError with no problems (programmer error)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "config: %d problem(s):", len(e.Errs))
	for _, err := range e.Errs {
		b.WriteString("\n  - ")
		b.WriteString(err.Error())
	}
	return b.String()
}

var allowedKinds = map[string]struct{}{
	"Deployment":  {},
	"HelmRelease": {},
	"Application": {},
}

// Validate runs every rule against f and returns *ValidationError
// if any rule failed, else nil. Rules are aggregated so an operator
// fixes all problems in one pass.
func (f *File) Validate() error {
	var errs []error

	if len(f.Components) == 0 {
		errs = append(errs, fmt.Errorf("config: no components defined"))
	}

	var dupOrder []string // ids in first-seen order, deduplicated
	idIndex := make(map[string][]int)
	for i, c := range f.Components {
		if c.Kind == "" {
			errs = append(errs, fmt.Errorf("components[%d]: kind is required", i))
		} else if _, ok := allowedKinds[c.Kind]; !ok {
			errs = append(errs, fmt.Errorf(
				"components[%d]: kind %q is not one of Deployment, HelmRelease, Application",
				i, c.Kind,
			))
		}
		if c.Namespace == "" {
			errs = append(errs, fmt.Errorf("components[%d]: namespace is required", i))
		}
		if c.Name == "" {
			errs = append(errs, fmt.Errorf("components[%d]: name is required", i))
		}
		for _, p := range []struct {
			name, val string
		}{
			{"kind", c.Kind},
			{"namespace", c.Namespace},
			{"name", c.Name},
		} {
			if strings.Contains(p.val, "/") {
				errs = append(errs, fmt.Errorf(
					"components[%d]: %s must not contain '/'", i, p.name,
				))
			}
		}

		// Only register fully-formed IDs to avoid noise like
		// `""/""/""` matching across empty rows.
		if c.Kind != "" && c.Namespace != "" && c.Name != "" {
			id := c.Kind + "/" + c.Namespace + "/" + c.Name
			if _, seen := idIndex[id]; !seen {
				dupOrder = append(dupOrder, id)
			}
			idIndex[id] = append(idIndex[id], i)
		}
	}

	for _, id := range dupOrder {
		indices := idIndex[id]
		if len(indices) > 1 {
			// Format as [0, 2] (comma-separated) for readability.
			parts := make([]string, len(indices))
			for j, idx := range indices {
				parts[j] = fmt.Sprintf("%d", idx)
			}
			errs = append(errs, fmt.Errorf(
				"duplicate component id %q at indices [%s]", id, strings.Join(parts, ", "),
			))
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return &ValidationError{Errs: errs}
}
