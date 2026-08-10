// Package catalog implements PebbleDB's durable relational system catalog.
package catalog

import (
	"bytes"
	"fmt"
	"pebbledb/codec"
	"sort"
	"strings"
	"unicode/utf8"
)

type DescriptorID uint64

type DatabaseDescriptor struct {
	ID   DescriptorID `json:"id"`
	Name string       `json:"name"`
}

type SchemaDescriptor struct {
	ID         DescriptorID `json:"id"`
	DatabaseID DescriptorID `json:"database_id"`
	Name       string       `json:"name"`
}

type IndexDescriptor struct {
	ID        DescriptorID `json:"id"`
	Name      string       `json:"name"`
	ColumnIDs []uint32     `json:"column_ids"`
	Unique    bool         `json:"unique"`
}

type ConstraintKind string

const (
	PrimaryKeyConstraint ConstraintKind = "PRIMARY_KEY"
	UniqueConstraint     ConstraintKind = "UNIQUE"
	CheckConstraint      ConstraintKind = "CHECK"
	ForeignKeyConstraint ConstraintKind = "FOREIGN_KEY"
)

type ConstraintDescriptor struct {
	ID                  DescriptorID   `json:"id"`
	Name                string         `json:"name"`
	Kind                ConstraintKind `json:"kind"`
	ColumnIDs           []uint32       `json:"column_ids,omitempty"`
	Expression          string         `json:"expression,omitempty"`
	ReferencedTableID   DescriptorID   `json:"referenced_table_id,omitempty"`
	ReferencedColumnIDs []uint32       `json:"referenced_column_ids,omitempty"`
}

type PartitionDescriptor struct {
	ID       DescriptorID `json:"id"`
	StartKey []byte       `json:"start_key,omitempty"`
	EndKey   []byte       `json:"end_key,omitempty"`
}

type TableDescriptor struct {
	ID          DescriptorID           `json:"id"`
	SchemaID    DescriptorID           `json:"schema_id"`
	Name        string                 `json:"name"`
	Version     uint64                 `json:"version"`
	Schema      codec.TableSchema      `json:"schema"`
	Indexes     []IndexDescriptor      `json:"indexes,omitempty"`
	Constraints []ConstraintDescriptor `json:"constraints,omitempty"`
	Partitions  []PartitionDescriptor  `json:"partitions,omitempty"`
}

func normalizeName(name string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(name))
	if normalized == "" {
		return "", fmt.Errorf("catalog: name cannot be empty")
	}
	if !utf8.ValidString(normalized) || strings.IndexByte(normalized, 0) >= 0 {
		return "", fmt.Errorf("catalog: name must be valid UTF-8 without NUL bytes")
	}
	if len(normalized) > 1024 {
		return "", fmt.Errorf("catalog: name is too long")
	}
	return normalized, nil
}

func validateDatabase(descriptor DatabaseDescriptor) error {
	if descriptor.ID == 0 {
		return fmt.Errorf("catalog: database ID must be positive")
	}
	name, err := normalizeName(descriptor.Name)
	if err != nil || name != descriptor.Name {
		return fmt.Errorf("catalog: database has an invalid normalized name")
	}
	return nil
}

func validateSchema(descriptor SchemaDescriptor) error {
	if descriptor.ID == 0 || descriptor.DatabaseID == 0 {
		return fmt.Errorf("catalog: schema and database IDs must be positive")
	}
	name, err := normalizeName(descriptor.Name)
	if err != nil || name != descriptor.Name {
		return fmt.Errorf("catalog: schema has an invalid normalized name")
	}
	return nil
}

func validateTable(descriptor TableDescriptor) error {
	if descriptor.ID == 0 || descriptor.SchemaID == 0 || descriptor.Version == 0 {
		return fmt.Errorf("catalog: table, schema, and version IDs must be positive")
	}
	name, err := normalizeName(descriptor.Name)
	if err != nil || name != descriptor.Name {
		return fmt.Errorf("catalog: table has an invalid normalized name")
	}
	if _, err := codec.EncodeTableSchema(descriptor.Schema); err != nil {
		return fmt.Errorf("catalog: invalid table schema: %w", err)
	}
	columns := make(map[uint32]struct{}, len(descriptor.Schema.Columns))
	for _, column := range descriptor.Schema.Columns {
		columns[column.ID] = struct{}{}
	}

	indexIDs := make(map[DescriptorID]struct{}, len(descriptor.Indexes))
	indexNames := make(map[string]struct{}, len(descriptor.Indexes))
	for _, index := range descriptor.Indexes {
		if index.ID == 0 || len(index.ColumnIDs) == 0 {
			return fmt.Errorf("catalog: index ID and columns are required")
		}
		indexName, err := normalizeName(index.Name)
		if err != nil || indexName != index.Name {
			return fmt.Errorf("catalog: index has an invalid normalized name")
		}
		if _, exists := indexIDs[index.ID]; exists {
			return fmt.Errorf("catalog: duplicate index ID %d", index.ID)
		}
		if _, exists := indexNames[index.Name]; exists {
			return fmt.Errorf("catalog: duplicate index name %q", index.Name)
		}
		indexIDs[index.ID], indexNames[index.Name] = struct{}{}, struct{}{}
		if err := validateColumnReferences(columns, index.ColumnIDs); err != nil {
			return fmt.Errorf("catalog: index %q: %w", index.Name, err)
		}
	}

	constraintIDs := make(map[DescriptorID]struct{}, len(descriptor.Constraints))
	constraintNames := make(map[string]struct{}, len(descriptor.Constraints))
	for _, constraint := range descriptor.Constraints {
		if constraint.ID == 0 {
			return fmt.Errorf("catalog: constraint ID must be positive")
		}
		constraintName, err := normalizeName(constraint.Name)
		if err != nil || constraintName != constraint.Name {
			return fmt.Errorf("catalog: constraint has an invalid normalized name")
		}
		if _, exists := constraintIDs[constraint.ID]; exists {
			return fmt.Errorf("catalog: duplicate constraint ID %d", constraint.ID)
		}
		if _, exists := constraintNames[constraint.Name]; exists {
			return fmt.Errorf("catalog: duplicate constraint name %q", constraint.Name)
		}
		constraintIDs[constraint.ID], constraintNames[constraint.Name] = struct{}{}, struct{}{}
		if err := validateConstraint(columns, constraint); err != nil {
			return err
		}
	}

	partitionIDs := make(map[DescriptorID]struct{}, len(descriptor.Partitions))
	for index, partition := range descriptor.Partitions {
		if partition.ID == 0 {
			return fmt.Errorf("catalog: partition ID must be positive")
		}
		if _, exists := partitionIDs[partition.ID]; exists {
			return fmt.Errorf("catalog: duplicate partition ID %d", partition.ID)
		}
		partitionIDs[partition.ID] = struct{}{}
		if len(partition.EndKey) > 0 && bytes.Compare(partition.StartKey, partition.EndKey) >= 0 {
			return fmt.Errorf("catalog: partition start must sort before end")
		}
		if index > 0 {
			previous := descriptor.Partitions[index-1]
			if len(previous.EndKey) == 0 || bytes.Compare(previous.EndKey, partition.StartKey) > 0 {
				return fmt.Errorf("catalog: partitions overlap or are not ordered")
			}
		}
	}
	return nil
}

func validateColumnReferences(columns map[uint32]struct{}, columnIDs []uint32) error {
	seen := make(map[uint32]struct{}, len(columnIDs))
	for _, columnID := range columnIDs {
		if _, exists := columns[columnID]; !exists {
			return fmt.Errorf("unknown column ID %d", columnID)
		}
		if _, exists := seen[columnID]; exists {
			return fmt.Errorf("duplicate column ID %d", columnID)
		}
		seen[columnID] = struct{}{}
	}
	return nil
}

func validateConstraint(columns map[uint32]struct{}, constraint ConstraintDescriptor) error {
	switch constraint.Kind {
	case PrimaryKeyConstraint, UniqueConstraint:
		if len(constraint.ColumnIDs) == 0 {
			return fmt.Errorf("catalog: constraint %q requires columns", constraint.Name)
		}
		return validateColumnReferences(columns, constraint.ColumnIDs)
	case CheckConstraint:
		if strings.TrimSpace(constraint.Expression) == "" {
			return fmt.Errorf("catalog: CHECK constraint %q requires an expression", constraint.Name)
		}
	case ForeignKeyConstraint:
		if len(constraint.ColumnIDs) == 0 || constraint.ReferencedTableID == 0 || len(constraint.ColumnIDs) != len(constraint.ReferencedColumnIDs) {
			return fmt.Errorf("catalog: FOREIGN KEY constraint %q is incomplete", constraint.Name)
		}
		return validateColumnReferences(columns, constraint.ColumnIDs)
	default:
		return fmt.Errorf("catalog: unknown constraint kind %q", constraint.Kind)
	}
	return nil
}

func sortedPartitions(partitions []PartitionDescriptor) []PartitionDescriptor {
	result := append([]PartitionDescriptor(nil), partitions...)
	sort.Slice(result, func(i, j int) bool {
		return bytes.Compare(result[i].StartKey, result[j].StartKey) < 0
	})
	return result
}
