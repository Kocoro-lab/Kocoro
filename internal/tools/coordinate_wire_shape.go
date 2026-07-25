package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// coordinateWireShape is deliberately limited to the versioned coordinate
// contracts. Legacy schema-0 AX payloads do not pass through this validator.
type coordinateWireShape struct {
	nullable bool
	fields   map[string]coordinateWireShape
	item     *coordinateWireShape
}

func coordinateScalarWireShape(nullable bool) coordinateWireShape {
	return coordinateWireShape{nullable: nullable}
}

func coordinateObjectWireShape(nullable bool, fields map[string]coordinateWireShape) coordinateWireShape {
	return coordinateWireShape{nullable: nullable, fields: fields}
}

func coordinateArrayWireShape(item coordinateWireShape) coordinateWireShape {
	return coordinateWireShape{item: &item}
}

func coordinateNullableWireShape(shape coordinateWireShape) coordinateWireShape {
	shape.nullable = true
	return shape
}

func validateCoordinateWireShape(label string, payload []byte, shape coordinateWireShape) error {
	trimmed := bytes.TrimSpace(payload)
	if bytes.Equal(trimmed, []byte("null")) {
		if shape.nullable {
			return nil
		}
		return fmt.Errorf("%s cannot be null", label)
	}
	if shape.fields != nil {
		var fields map[string]json.RawMessage
		if err := decodeStrictCoordinateJSON(trimmed, &fields); err != nil {
			return fmt.Errorf("decode %s object shape: %w", label, err)
		}
		if len(fields) != len(shape.fields) {
			return fmt.Errorf("%s must contain exactly %d fields", label, len(shape.fields))
		}
		for name, fieldShape := range shape.fields {
			value, exists := fields[name]
			if !exists {
				return fmt.Errorf("%s missing field %q", label, name)
			}
			if err := validateCoordinateWireShape(label+"."+name, value, fieldShape); err != nil {
				return err
			}
		}
		return nil
	}
	if shape.item != nil {
		var items []json.RawMessage
		if err := decodeStrictCoordinateJSON(trimmed, &items); err != nil {
			return fmt.Errorf("decode %s array shape: %w", label, err)
		}
		for index, item := range items {
			if err := validateCoordinateWireShape(fmt.Sprintf("%s[%d]", label, index), item, *shape.item); err != nil {
				return err
			}
		}
	}
	return nil
}

var coordinateTopologyRefWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"topology_id": coordinateScalarWireShape(false),
	"generation":  coordinateScalarWireShape(false),
})

var coordinateQuartzRectWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"x":      coordinateScalarWireShape(false),
	"y":      coordinateScalarWireShape(false),
	"width":  coordinateScalarWireShape(false),
	"height": coordinateScalarWireShape(false),
})

var coordinatePixelRectWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"x":      coordinateScalarWireShape(false),
	"y":      coordinateScalarWireShape(false),
	"width":  coordinateScalarWireShape(false),
	"height": coordinateScalarWireShape(false),
})

var coordinateAffineWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"a":  coordinateScalarWireShape(false),
	"b":  coordinateScalarWireShape(false),
	"c":  coordinateScalarWireShape(false),
	"d":  coordinateScalarWireShape(false),
	"tx": coordinateScalarWireShape(false),
	"ty": coordinateScalarWireShape(false),
})
