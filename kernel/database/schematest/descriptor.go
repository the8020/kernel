package schematest

import (
	"encoding/json"
	"the8020/kernel/database"
)

type DefaultDescriptor struct {
	Kind  string `json:"kind"`
	Value any    `json:"value,omitempty"`
}

type ReferenceDescriptor struct {
	Table  string `json:"table"`
	Column string `json:"column"`
}

type ColumnDescriptor struct {
	Name        string               `json:"name"`
	LogicalType string               `json:"logical_type"`
	Precision   int                  `json:"precision,omitempty"`
	Scale       int                  `json:"scale,omitempty"`
	EnumValues  []string             `json:"enum_values,omitempty"`
	Nullable    bool                 `json:"nullable"`
	Default     *DefaultDescriptor   `json:"default,omitempty"`
	Generated   bool                 `json:"generated"`
	PrimaryKey  bool                 `json:"primary_key"`
	Unique      bool                 `json:"unique"`
	Reference   *ReferenceDescriptor `json:"reference,omitempty"`
}

type IndexDescriptor struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Unique  bool     `json:"unique"`
}

type Descriptor struct {
	FormatVersion int                `json:"format_version"`
	TableID       string             `json:"table_id"`
	Columns       []ColumnDescriptor `json:"columns"`
	PrimaryKey    []string           `json:"primary_key"`
	Indexes       []IndexDescriptor  `json:"indexes"`
}

func Transport(value Descriptor) database.TableDescriptor {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var result database.TableDescriptor
	if err := json.Unmarshal(encoded, &result); err != nil {
		panic(err)
	}
	return result
}
func Decode(value database.TableDescriptor) Descriptor {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var result Descriptor
	if err := json.Unmarshal(encoded, &result); err != nil {
		panic(err)
	}
	return result
}
