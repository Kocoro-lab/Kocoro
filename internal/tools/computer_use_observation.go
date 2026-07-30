package tools

import (
	"encoding/json"
	"errors"
	"fmt"
)

var (
	errComputerUseFingerprintNotFound  = errors.New("computer-use fingerprint not found")
	errComputerUseFingerprintAmbiguous = errors.New("computer-use fingerprint is ambiguous")
)

type computerUseFrame struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

type computerUseElement struct {
	Ref           string               `json:"ref"`
	Fingerprint   string               `json:"fingerprint,omitempty"`
	Path          string               `json:"path,omitempty"`
	Role          string               `json:"role"`
	Subrole       *string              `json:"subrole,omitempty"`
	Identifier    *string              `json:"identifier,omitempty"`
	Title         *string              `json:"title,omitempty"`
	Description   *string              `json:"description,omitempty"`
	Desc          *string              `json:"desc,omitempty"`
	Value         *string              `json:"value,omitempty"`
	ValueRedacted bool                 `json:"value_redacted"`
	Enabled       *bool                `json:"enabled,omitempty"`
	Focused       bool                 `json:"focused"`
	Selected      bool                 `json:"selected"`
	Actions       []string             `json:"actions,omitempty"`
	Frame         *computerUseFrame    `json:"frame,omitempty"`
	Children      []computerUseElement `json:"children,omitempty"`
}

type computerUseRefPath struct {
	Path        string `json:"path"`
	Role        string `json:"role"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

type computerUseTree struct {
	SchemaVersion int                           `json:"schema_version,omitempty"`
	AppName       string                        `json:"app_name,omitempty"`
	BundleID      string                        `json:"bundle_id,omitempty"`
	PID           int                           `json:"pid"`
	WindowTitle   string                        `json:"window_title,omitempty"`
	WindowID      *int                          `json:"window_id,omitempty"`
	WindowFrame   *computerUseFrame             `json:"window_frame,omitempty"`
	FocusedRef    *string                       `json:"focused_ref,omitempty"`
	Elements      []computerUseElement          `json:"elements"`
	RefPaths      map[string]computerUseRefPath `json:"ref_paths"`

	// Migration aliases retained on the wire until the legacy accessibility
	// consumer and older packaged daemons have moved to the v1 names.
	App    string `json:"app"`
	Window string `json:"window"`
}

func decodeComputerUseTree(payload []byte) (computerUseTree, error) {
	var tree computerUseTree
	if err := json.Unmarshal(payload, &tree); err != nil {
		return computerUseTree{}, err
	}
	if tree.SchemaVersion < 0 || tree.SchemaVersion > 1 {
		return computerUseTree{}, fmt.Errorf("unsupported accessibility schema_version %d", tree.SchemaVersion)
	}
	if tree.AppName == "" {
		tree.AppName = tree.App
	}
	if tree.App == "" {
		tree.App = tree.AppName
	}
	if tree.WindowTitle == "" {
		tree.WindowTitle = tree.Window
	}
	if tree.Window == "" {
		tree.Window = tree.WindowTitle
	}
	if tree.RefPaths == nil {
		tree.RefPaths = make(map[string]computerUseRefPath)
	}
	if err := validateComputerUseElements(
		tree.Elements,
		tree.SchemaVersion == 1,
		tree.RefPaths,
		make(map[string]struct{}),
	); err != nil {
		return computerUseTree{}, err
	}
	if tree.SchemaVersion == 1 && len(tree.RefPaths) != countComputerUseElements(tree.Elements) {
		return computerUseTree{}, fmt.Errorf("accessibility ref_paths contains entries without typed elements")
	}
	return tree, nil
}

func validateComputerUseElements(
	elements []computerUseElement,
	requireTypedFields bool,
	refPaths map[string]computerUseRefPath,
	seenRefs map[string]struct{},
) error {
	for _, element := range elements {
		if requireTypedFields {
			switch {
			case element.Ref == "":
				return errors.New("accessibility element omitted required ref")
			case element.Fingerprint == "":
				return fmt.Errorf("accessibility element %q omitted required fingerprint", element.Ref)
			case element.Path == "":
				return fmt.Errorf("accessibility element %q omitted required path", element.Ref)
			case element.Role == "":
				return fmt.Errorf("accessibility element %q omitted required role", element.Ref)
			case element.Enabled == nil:
				return fmt.Errorf("accessibility element %q omitted required enabled state", element.Ref)
			}
			if _, duplicate := seenRefs[element.Ref]; duplicate {
				return fmt.Errorf("accessibility element ref %q is duplicated", element.Ref)
			}
			seenRefs[element.Ref] = struct{}{}
			entry, exists := refPaths[element.Ref]
			if !exists {
				return fmt.Errorf("accessibility element %q is missing from ref_paths", element.Ref)
			}
			if entry.Path != element.Path || entry.Role != element.Role || entry.Fingerprint != element.Fingerprint {
				return fmt.Errorf("accessibility ref_paths entry %q disagrees with typed element", element.Ref)
			}
		}
		if element.ValueRedacted && element.Value != nil {
			return fmt.Errorf("accessibility element %q contains a value marked redacted", element.Ref)
		}
		if err := validateComputerUseElements(element.Children, requireTypedFields, refPaths, seenRefs); err != nil {
			return err
		}
	}
	return nil
}

func countComputerUseElements(elements []computerUseElement) int {
	count := 0
	for _, element := range elements {
		count += 1 + countComputerUseElements(element.Children)
	}
	return count
}

func resolveComputerUseFingerprint(elements []computerUseElement, fingerprint string) (computerUseElement, error) {
	var matches []computerUseElement
	var walk func([]computerUseElement)
	walk = func(nodes []computerUseElement) {
		for _, node := range nodes {
			if node.Fingerprint == fingerprint {
				matches = append(matches, node)
			}
			walk(node.Children)
		}
	}
	walk(elements)
	switch len(matches) {
	case 0:
		return computerUseElement{}, errComputerUseFingerprintNotFound
	case 1:
		return matches[0], nil
	default:
		return computerUseElement{}, errComputerUseFingerprintAmbiguous
	}
}
