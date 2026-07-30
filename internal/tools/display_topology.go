package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"time"
)

type DisplayTopologyRectV1 struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

type DisplayTopologyDisplayV1 struct {
	DisplayID             uint32                `json:"display_id"`
	IsMain                bool                  `json:"is_main"`
	IsBuiltin             bool                  `json:"is_builtin"`
	IsActive              bool                  `json:"is_active"`
	IsOnline              bool                  `json:"is_online"`
	IsAsleep              bool                  `json:"is_asleep"`
	QuartzBounds          DisplayTopologyRectV1 `json:"quartz_bounds"`
	AppKitFrame           DisplayTopologyRectV1 `json:"appkit_frame"`
	AppKitVisibleFrame    DisplayTopologyRectV1 `json:"appkit_visible_frame"`
	BackingScaleFactor    float64               `json:"backing_scale_factor"`
	PixelWidth            int                   `json:"pixel_width"`
	PixelHeight           int                   `json:"pixel_height"`
	RotationDegrees       float64               `json:"rotation_degrees"`
	MirrorMasterDisplayID *uint32               `json:"mirror_master_display_id"`
}

type DisplayTopologyV1 struct {
	SchemaVersion int                        `json:"schema_version"`
	TopologyID    string                     `json:"topology_id"`
	HelperBootID  string                     `json:"helper_boot_id"`
	Generation    uint64                     `json:"generation"`
	CapturedAt    string                     `json:"captured_at"`
	MainDisplayID uint32                     `json:"main_display_id"`
	Displays      []DisplayTopologyDisplayV1 `json:"displays"`
}

func DecodeDisplayTopologyV1(payload []byte) (DisplayTopologyV1, error) {
	if err := validateCoordinateWireShape("display topology v1", payload, displayTopologyWireShapeV1); err != nil {
		return DisplayTopologyV1{}, err
	}
	var topology DisplayTopologyV1
	if err := decodeStrictCoordinateJSON(payload, &topology); err != nil {
		return DisplayTopologyV1{}, fmt.Errorf("decode display topology v1: %w", err)
	}
	if err := topology.Validate(); err != nil {
		return DisplayTopologyV1{}, err
	}
	return topology, nil
}

var displayTopologyDisplayWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"display_id":               coordinateScalarWireShape(false),
	"is_main":                  coordinateScalarWireShape(false),
	"is_builtin":               coordinateScalarWireShape(false),
	"is_active":                coordinateScalarWireShape(false),
	"is_online":                coordinateScalarWireShape(false),
	"is_asleep":                coordinateScalarWireShape(false),
	"quartz_bounds":            coordinateQuartzRectWireShapeV1,
	"appkit_frame":             coordinateQuartzRectWireShapeV1,
	"appkit_visible_frame":     coordinateQuartzRectWireShapeV1,
	"backing_scale_factor":     coordinateScalarWireShape(false),
	"pixel_width":              coordinateScalarWireShape(false),
	"pixel_height":             coordinateScalarWireShape(false),
	"rotation_degrees":         coordinateScalarWireShape(false),
	"mirror_master_display_id": coordinateScalarWireShape(true),
})

var displayTopologyWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"schema_version":  coordinateScalarWireShape(false),
	"topology_id":     coordinateScalarWireShape(false),
	"helper_boot_id":  coordinateScalarWireShape(false),
	"generation":      coordinateScalarWireShape(false),
	"captured_at":     coordinateScalarWireShape(false),
	"main_display_id": coordinateScalarWireShape(false),
	"displays":        coordinateArrayWireShape(displayTopologyDisplayWireShapeV1),
})

func (topology DisplayTopologyV1) Validate() error {
	if topology.SchemaVersion != 1 {
		return fmt.Errorf("unsupported display topology schema_version %d", topology.SchemaVersion)
	}
	if topology.TopologyID == "" || topology.HelperBootID == "" || topology.Generation == 0 || topology.MainDisplayID == 0 {
		return fmt.Errorf("display topology authority is required")
	}
	if _, err := time.Parse(time.RFC3339Nano, topology.CapturedAt); err != nil {
		return fmt.Errorf("display topology captured_at must be RFC3339: %w", err)
	}
	if len(topology.Displays) == 0 {
		return fmt.Errorf("display topology must contain at least one display")
	}

	displayIDs := make(map[uint32]struct{}, len(topology.Displays))
	mainCount := 0
	mainMatchesAuthority := false
	for _, display := range topology.Displays {
		if display.DisplayID == 0 {
			return fmt.Errorf("display_id must not be zero")
		}
		if _, exists := displayIDs[display.DisplayID]; exists {
			return fmt.Errorf("duplicate display_id %d", display.DisplayID)
		}
		displayIDs[display.DisplayID] = struct{}{}
		if display.IsMain {
			mainCount++
			mainMatchesAuthority = display.DisplayID == topology.MainDisplayID
		}
		for field, rect := range map[string]DisplayTopologyRectV1{
			"quartz_bounds":        display.QuartzBounds,
			"appkit_frame":         display.AppKitFrame,
			"appkit_visible_frame": display.AppKitVisibleFrame,
		} {
			if err := validateDisplayTopologyRect(field, rect); err != nil {
				return fmt.Errorf("display %d: %w", display.DisplayID, err)
			}
		}
		if !finiteCoordinate(display.BackingScaleFactor) || display.BackingScaleFactor <= 0 {
			return fmt.Errorf("display %d backing_scale_factor must be positive and finite", display.DisplayID)
		}
		if !coordinateApproximatelyEqual(display.QuartzBounds.Width, display.AppKitFrame.Width) ||
			!coordinateApproximatelyEqual(display.QuartzBounds.Height, display.AppKitFrame.Height) {
			return fmt.Errorf("display %d Quartz/AppKit logical sizes must match", display.DisplayID)
		}
		if display.PixelWidth <= 0 || display.PixelHeight <= 0 {
			return fmt.Errorf("display %d pixel sizes must be positive", display.DisplayID)
		}
		if !finiteCoordinate(display.RotationDegrees) || display.RotationDegrees < 0 || display.RotationDegrees >= 360 {
			return fmt.Errorf("display %d rotation_degrees must be finite and in [0, 360)", display.DisplayID)
		}
		if display.MirrorMasterDisplayID != nil {
			if *display.MirrorMasterDisplayID == 0 {
				return fmt.Errorf("display %d mirror master must not be zero", display.DisplayID)
			}
			if *display.MirrorMasterDisplayID == display.DisplayID {
				return fmt.Errorf("display %d cannot mirror itself", display.DisplayID)
			}
		}
	}
	if mainCount != 1 || !mainMatchesAuthority {
		return fmt.Errorf("main_display_id must identify the unique main display")
	}
	for _, display := range topology.Displays {
		if display.MirrorMasterDisplayID != nil {
			if _, exists := displayIDs[*display.MirrorMasterDisplayID]; !exists {
				return fmt.Errorf("display %d mirror master %d is not in this topology", display.DisplayID, *display.MirrorMasterDisplayID)
			}
		}
	}
	return nil
}

func validateDisplayTopologyRect(field string, rect DisplayTopologyRectV1) error {
	if !finiteCoordinate(rect.X) || !finiteCoordinate(rect.Y) ||
		!finiteCoordinate(rect.Width) || !finiteCoordinate(rect.Height) {
		return fmt.Errorf("%s must be finite", field)
	}
	if rect.Width <= 0 || rect.Height <= 0 {
		return fmt.Errorf("%s sizes must be positive", field)
	}
	return nil
}

func finiteCoordinate(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func coordinateApproximatelyEqual(left, right float64) bool {
	return math.Abs(left-right) <= 0.000001
}

func decodeStrictCoordinateJSON(payload []byte, target any) error {
	if err := rejectDuplicateCoordinateJSONMembers(payload); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return fmt.Errorf("trailing data: %w", err)
	}
	return nil
}

func rejectDuplicateCoordinateJSONMembers(payload []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := scanCoordinateJSONValue(decoder); err != nil {
		return fmt.Errorf("invalid coordinate JSON: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("invalid coordinate JSON: trailing JSON value")
		}
		return fmt.Errorf("invalid coordinate JSON trailing data: %w", err)
	}
	return nil
}

func scanCoordinateJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}

	switch delimiter {
	case '{':
		members := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("JSON object member must be a string")
			}
			if _, duplicate := members[key]; duplicate {
				return fmt.Errorf("duplicate JSON object member %q", key)
			}
			members[key] = struct{}{}
			if err := scanCoordinateJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return fmt.Errorf("invalid JSON object closing delimiter")
		}
	case '[':
		for decoder.More() {
			if err := scanCoordinateJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return fmt.Errorf("invalid JSON array closing delimiter")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
}
